// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Smana/agent-platform/internal/envelope"
)

// R17, R18, M5, B3: an event goes to each subscriber of its repository and type, signed; a room
// that existed before the subscriber starts at its last seq, a later one at its first event; a
// room whose repository is not known yet holds its cursor until it is; a failure is retried three
// times, then given up; three given-up deliveries in a row disable the subscriber, whose cursor
// still advances.
func TestEmitter(t *testing.T) {
	var hits atomic.Int32
	var fail atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		if _, err := Verify([][]byte{k1}, r.Header, body, time.Now()); err != nil {
			t.Error("unsigned delivery")
		}
		if fail.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	rig := newEmitterRig(t, srv.URL+"/webhooks", []string{"room.ended", "room.pr_opened"})
	rig.log("aaaaaaaa", fact(1, factsPhase("Received"))) // before the subscriber existed
	rig.start()
	rig.log("aaaaaaaa", fact(2, factsPR("AwaitingCI", 7)))
	rig.waitCursor(t, "aaaaaaaa", 2)
	if hits.Load() != 1 {
		t.Fatalf("one pr_opened, no replay of seq 1: %d", hits.Load())
	}
	rig.withRepo("bbbbbbbb", "Smana/other")
	rig.log("bbbbbbbb", fact(1, factsPR("AwaitingCI", 9)))
	rig.waitCursor(t, "bbbbbbbb", 1)
	if hits.Load() != 1 {
		t.Fatal("another repository's event was delivered")
	}
	rig.withRepo("gggggggg", "") // its Room is not in the cache yet
	rig.log("gggggggg", fact(1, factsPhase("Done")))
	time.Sleep(50 * time.Millisecond) // five sweeps of the rig's 10 ms
	if rig.cursor("gggggggg") != 0 || hits.Load() != 1 {
		t.Fatal("an event of a room with no known repository was given up or delivered")
	}
	rig.withRepo("gggggggg", "Smana/ai-native-ref")
	rig.waitCursor(t, "gggggggg", 1)
	if hits.Load() != 2 {
		t.Fatalf("the room's event once its repository is known: %d", hits.Load())
	}
	fail.Store(true)
	base := hits.Load()
	for i, room := range []string{"cccccccc", "dddddddd", "eeeeeeee"} {
		rig.log(room, fact(1, factsPhase("Done")))
		rig.stepRetries(t) // the fake sleep is asked 10 s, 1 min, 5 min
		rig.waitCursor(t, room, 1)
		if want := base + int32(4*(i+1)); hits.Load() != want {
			t.Fatalf("after %s: %d attempts, want %d (one try, three retries)", room, hits.Load(), want)
		}
	}
	if !rig.disabled("slack-app") || rig.gauge("slack-app") != 1 {
		t.Fatal("three failed deliveries disable the subscriber")
	}
	before := hits.Load()
	rig.log("ffffffff", fact(1, factsPhase("Done")))
	rig.waitCursor(t, "ffffffff", 1)
	if hits.Load() != before {
		t.Fatal("a disabled subscriber received a delivery")
	}
}

// A notification lost while the listener was down is made up on the next ready (the planner's
// Lagging on reconnect): the room is past its cursor, and nothing notified it.
func TestEmitterCatchesUpOnReady(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	rig := newEmitterRig(t, srv.URL, []string{"room.ended"})
	rig.log("aaaaaaaa", fact(1, factsPhase("Received")))
	rig.start()
	rig.silent("aaaaaaaa", fact(2, factsPhase("Done"))) // appended while the listener was down
	rig.reconnect()
	rig.waitCursor(t, "aaaaaaaa", 2)
	if hits.Load() != 1 {
		t.Fatalf("the missed event was delivered once: %d", hits.Load())
	}
}

// M7: a subscriber whose endpoint is dead and waiting out its retries never holds another back.
func TestEmitterSubscribersAreIndependent(t *testing.T) {
	var good atomic.Int32
	live := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		good.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer live.Close()
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	defer dead.Close()
	rig := newEmitterRig(t, dead.URL, []string{"room.ended"})
	rig.addSubscriber("second", live.URL, []string{"room.ended"})
	rig.start()
	rig.log("aaaaaaaa", fact(1, factsPhase("Done")))
	// slack-app's worker now waits on the unbuffered fake sleep, which nobody receives.
	rig.waitCursorOf(t, "second", "aaaaaaaa", 1)
	if good.Load() != 1 {
		t.Fatalf("the live subscriber got %d deliveries", good.Load())
	}
	if rig.cursorOf("slack-app", "aaaaaaaa") != 0 {
		t.Fatal("the dead subscriber's cursor moved before its retries ended")
	}
}

// emitterRig fakes everything the emitter reads and writes.
type emitterRig struct {
	t    *testing.T
	em   *Emitter
	ctx  context.Context
	mu   sync.Mutex
	logs map[string][]envelope.Event
	repo map[string]string
	cur  map[[2]string]int64
	fail map[string]int
	off  map[string]bool
	reg  map[string]bool
	dis  map[string]int // the last WebhookDisabled value per subscriber
	note chan [2]any
	ups  chan struct{} // one value per ready
	slp  chan time.Duration
}

func newEmitterRig(t *testing.T, url string, events []string) *emitterRig {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r := &emitterRig{t: t, ctx: ctx, logs: map[string][]envelope.Event{}, repo: map[string]string{}, cur: map[[2]string]int64{},
		fail: map[string]int{}, off: map[string]bool{}, reg: map[string]bool{}, dis: map[string]int{},
		note: make(chan [2]any, 100), ups: make(chan struct{}, 10), slp: make(chan time.Duration)}
	r.em = &Emitter{Events: r, Rooms: r.roomRepo, State: r, Metrics: r, Now: time.Now, Sleep: r.sleep,
		SweepEvery: 10 * time.Millisecond}
	r.addSubscriber("slack-app", url, events)
	return r
}

// addSubscriber declares a subscriber of Smana/ai-native-ref on the cluster network: SafeClient
// refuses the httptest server's 127.0.0.1 (L4).
func (r *emitterRig) addSubscriber(name, url string, events []string) {
	r.t.Helper()
	key := filepath.Join(r.t.TempDir(), name+".key")
	if err := os.WriteFile(key, []byte(base64.StdEncoding.EncodeToString(k1)+"\n"), 0o600); err != nil {
		r.t.Fatal(err)
	}
	r.em.Subs = append(r.em.Subs, Subscriber{Name: name, Network: "cluster", URL: url, SecretFile: key, Events: events,
		Repositories: []string{"Smana/ai-native-ref"}})
}

func (r *emitterRig) start() {
	r.t.Helper()
	go func() { _ = r.em.Start(r.ctx) }()
	select {
	case <-r.ups:
	case <-time.After(5 * time.Second):
		r.t.Fatal("the emitter never listened")
	}
}

// reconnect makes the fake Listen return an error, then call ready again.
func (r *emitterRig) reconnect() {
	r.t.Helper()
	r.note <- [2]any{"drop", int64(0)}
	select {
	case <-r.ups:
	case <-time.After(10 * time.Second):
		r.t.Fatal("the emitter never reconnected")
	}
}

func (r *emitterRig) withRepo(room, repo string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.repo[room] = repo
}

func (r *emitterRig) roomRepo(_ context.Context, room string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if repo, ok := r.repo[room]; ok {
		return repo, nil
	}
	return "Smana/ai-native-ref", nil
}

// silent appends without a notification: the listener was down.
func (r *emitterRig) silent(room string, ev envelope.Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ev.RoomID = room
	r.logs[room] = append(r.logs[room], ev)
}

func (r *emitterRig) log(room string, ev envelope.Event) {
	r.silent(room, ev)
	r.note <- [2]any{room, ev.Seq}
}

// Range is LogReader's.
func (r *emitterRig) Range(_ context.Context, room string, after int64, limit int) ([]envelope.Event, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []envelope.Event
	for _, ev := range r.logs[room] {
		if ev.Seq > after && len(out) < limit {
			out = append(out, ev)
		}
	}
	return out, nil
}

// Listen is LogReader's: ready, then each notification, until ctx ends or a "drop" is sent.
func (r *emitterRig) Listen(ctx context.Context, ready func(), fn func(string, int64)) error {
	ready()
	r.ups <- struct{}{}
	for {
		select {
		case <-ctx.Done():
			return nil
		case n := <-r.note:
			if n[0] == "drop" {
				return context.DeadlineExceeded
			}
			fn(n[0].(string), n[1].(int64))
		}
	}
}

func (r *emitterRig) last(room string) int64 {
	if l := r.logs[room]; len(l) > 0 {
		return l[len(l)-1].Seq
	}
	return 0
}

// Subscriber is StateStore's: the first call for a name puts its cursor in every room the log
// holds at that room's last seq (B3).
func (r *emitterRig) Subscriber(_ context.Context, sub string, _ int) (int, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.reg[sub] {
		r.reg[sub] = true
		for room := range r.logs {
			r.cur[[2]string{sub, room}] = r.last(room)
		}
	}
	return r.fail[sub], r.off[sub], nil
}

func (r *emitterRig) WebhookCursor(_ context.Context, sub, room string) (int64, error) {
	return r.cursorOf(sub, room), nil
}

func (r *emitterRig) cursorOf(sub, room string) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cur[[2]string{sub, room}]
}

func (r *emitterRig) SetWebhookCursor(_ context.Context, sub, room string, seq int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if k := [2]string{sub, room}; seq > r.cur[k] {
		r.cur[k] = seq
	}
	return nil
}

func (r *emitterRig) Lagging(_ context.Context, sub string, limit int) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for room := range r.logs {
		if r.last(room) > r.cur[[2]string{sub, room}] && len(out) < limit {
			out = append(out, room)
		}
	}
	return out, nil
}

func (r *emitterRig) Failed(_ context.Context, sub string) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fail[sub]++
	return r.fail[sub], nil
}

func (r *emitterRig) Succeeded(_ context.Context, sub string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fail[sub] = 0
	return nil
}

func (r *emitterRig) Disable(_ context.Context, sub string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.off[sub] = true
	return nil
}

func (r *emitterRig) WebhookDelivery(context.Context, string, string) {}

func (r *emitterRig) WebhookDisabled(_ context.Context, sub string, disabled bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.dis[sub] = 0
	if disabled {
		r.dis[sub] = 1
	}
}

func (r *emitterRig) sleep(ctx context.Context, d time.Duration) error {
	select {
	case r.slp <- d:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// stepRetries releases the three waits of one given-up delivery, checking each duration.
func (r *emitterRig) stepRetries(t *testing.T) {
	t.Helper()
	var got []time.Duration
	for range 3 {
		select {
		case d := <-r.slp:
			got = append(got, d)
		case <-time.After(5 * time.Second):
			t.Fatalf("waited for %v of the 3 retry sleeps", got)
		}
	}
	if want := []time.Duration{10 * time.Second, time.Minute, 5 * time.Minute}; !slices.Equal(got, want) {
		t.Fatalf("retry waits %v, want %v", got, want)
	}
}

func (r *emitterRig) waitCursor(t *testing.T, room string, seq int64) {
	t.Helper()
	r.waitCursorOf(t, "slack-app", room, seq)
}

func (r *emitterRig) waitCursorOf(t *testing.T, sub, room string, seq int64) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if r.cursorOf(sub, room) >= seq {
			return
		}
	}
	t.Fatalf("%s's cursor in %s is %d, want %d", sub, room, r.cursorOf(sub, room), seq)
}

func (r *emitterRig) cursor(room string) int64 { return r.cursorOf("slack-app", room) }

func (r *emitterRig) disabled(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.off[name]
}

func (r *emitterRig) gauge(name string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.dis[name]
}

func factsPhase(p string) envelope.TaskFacts { return envelope.TaskFacts{Phase: p} }

func factsPR(p string, n int) envelope.TaskFacts {
	f := factsPhase(p)
	f.PR = &envelope.PRFact{Number: n, URL: "https://github.com/Smana/ai-native-ref/pull/" + strconv.Itoa(n)}
	return f
}
