// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"context"
	"encoding/base64"
	"errors"
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
	rig.log("bbbbbbbb", fact(1, factsPhase("Done"))) // no URL in the log: the Room says Smana/other
	rig.waitCursor(t, "bbbbbbbb", 1)
	if hits.Load() != 1 {
		t.Fatal("another repository's event was delivered")
	}
	rig.withRepo("gggggggg", "") // its Room is not in the cache yet
	rig.log("gggggggg", fact(1, factsPhase("Done")))
	waitFor(t, "five sweeps to ask for the room's repository", func() bool { return rig.asked("gggggggg") >= 5 })
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
	t      *testing.T
	em     *Emitter
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	logs   map[string][]envelope.Event
	repo   map[string]string
	cur    map[[2]string]int64
	fail   map[string]int
	off    map[string]bool
	reg    map[string]bool
	dis    map[string]int // the last WebhookDisabled value per subscriber
	note   chan [2]any
	ups    chan struct{} // one value per ready
	slp    chan time.Duration

	calls    map[string]int // how often roomRepo was asked about each room
	outcomes map[string]int // "subscriber/outcome" -> count
	subErrs  int            // the next Subscriber calls that fail
	lagErrs  int            // the next Lagging calls that fail
	lagCalls int
	done     chan struct{} // closed when Start returned
}

func newEmitterRig(t *testing.T, url string, events []string) *emitterRig {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	r := &emitterRig{t: t, ctx: ctx, cancel: cancel, logs: map[string][]envelope.Event{}, repo: map[string]string{}, cur: map[[2]string]int64{},
		fail: map[string]int{}, off: map[string]bool{}, reg: map[string]bool{}, dis: map[string]int{},
		calls: map[string]int{}, outcomes: map[string]int{}, note: make(chan [2]any, 100), ups: make(chan struct{}, 10), slp: make(chan time.Duration)}
	r.em = &Emitter{Events: r, Rooms: r.roomRepo, State: r, Metrics: r, Now: time.Now, Sleep: r.sleep,
		FactoryPrincipal: "system:factory", SweepEvery: 10 * time.Millisecond, MinBackoff: 5 * time.Millisecond}
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
	r.done = make(chan struct{})
	go func() { defer close(r.done); _ = r.em.Start(r.ctx) }()
	r.t.Cleanup(func() {
		r.cancel()
		select {
		case <-r.done:
		case <-time.After(5 * time.Second):
			r.t.Error("Start did not return after its context ended")
		}
	})
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
	r.calls[room]++
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
	if r.subErrs > 0 {
		r.subErrs--
		return 0, false, errors.New("database blip")
	}
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
	r.lagCalls++
	if r.lagErrs > 0 {
		r.lagErrs--
		return nil, errors.New("database blip")
	}
	var out []string
	for room := range r.logs {
		if r.last(room) > r.cur[[2]string{sub, room}] {
			out = append(out, room)
		}
	}
	slices.Sort(out)
	return out[:min(len(out), limit)], nil
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

func (r *emitterRig) WebhookDelivery(_ context.Context, sub, outcome string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.outcomes[sub+"/"+outcome]++
}

func (r *emitterRig) outcome(sub, outcome string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.outcomes[sub+"/"+outcome]
}

func (r *emitterRig) asked(room string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls[room]
}

// waitFor polls cond every 5 ms for up to 5 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

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

func seal(seq int64) envelope.Event {
	return envelope.Event{Seq: seq, Type: envelope.StateChanged, Origin: envelope.OriginBroker,
		Actor:   envelope.Actor{Kind: envelope.ActorSystem, ID: "system:room-broker"},
		Payload: envelope.StatePayload("room_phase", map[string]any{"phase": "Closed", "reason": "done"})}
}

func counting(hits *atomic.Int32) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
}

// C1: the normal close deletes the Room right after the seal, but the log still names the
// repository (its pull request's URL), so the room's last events are delivered.
func TestEmitterDeliversAClosedRoomsEnd(t *testing.T) {
	var hits atomic.Int32
	srv := counting(&hits)
	defer srv.Close()
	rig := newEmitterRig(t, srv.URL, []string{"room.ended"})
	rig.withRepo("hhhhhhhh", "") // the Room is gone
	rig.start()
	rig.log("hhhhhhhh", fact(1, factsPR("AwaitingCI", 3)))
	rig.log("hhhhhhhh", fact(2, factsPhase("Done")))
	rig.log("hhhhhhhh", seal(3))
	rig.waitCursor(t, "hhhhhhhh", 3)
	if hits.Load() != 1 {
		t.Fatalf("room.ended of a deleted Room: %d deliveries", hits.Load())
	}
}

// C1: a sealed room whose repository nothing names moves on and is counted, instead of staying
// dirty forever; an unsealed one is held.
func TestEmitterSealedRoomWithoutRepository(t *testing.T) {
	var hits atomic.Int32
	srv := counting(&hits)
	defer srv.Close()
	rig := newEmitterRig(t, srv.URL, []string{"room.ended"})
	rig.withRepo("iiiiiiii", "")
	rig.withRepo("jjjjjjjj", "")
	rig.start()
	rig.log("jjjjjjjj", fact(1, factsPhase("Done"))) // open: held
	rig.log("iiiiiiii", fact(1, factsPhase("Done")))
	rig.log("iiiiiiii", seal(2))
	rig.waitCursor(t, "iiiiiiii", 2)
	if hits.Load() != 0 || rig.outcome("slack-app", "unresolved") != 1 {
		t.Fatalf("%d deliveries, %d unresolved", hits.Load(), rig.outcome("slack-app", "unresolved"))
	}
	waitFor(t, "sweeps over the open room", func() bool { return rig.asked("jjjjjjjj") >= 3 })
	if rig.cursor("jjjjjjjj") != 0 || rig.outcome("slack-app", "unresolved") != 1 {
		t.Fatal("an open room with no known repository is held, not given up")
	}
}

// I1: a Lagging that failed on ready is retried by the sweep, and a list cut at the limit is
// asked for again until it is short.
func TestEmitterCatchUpRetries(t *testing.T) {
	var hits atomic.Int32
	srv := counting(&hits)
	defer srv.Close()
	rig := newEmitterRig(t, srv.URL, []string{"room.ended"})
	rig.em.LaggingLimit = 2
	rig.log("aaaaaaaa", fact(1, factsPhase("Received")))
	rig.lagErrs = 3
	rig.start()
	for _, room := range []string{"kkkkkkkk", "llllllll", "mmmmmmmm"} {
		rig.silent(room, fact(1, factsPhase("Done"))) // appended while the listener was down
	}
	for _, room := range []string{"kkkkkkkk", "llllllll", "mmmmmmmm"} {
		rig.waitCursor(t, room, 1)
	}
	if hits.Load() != 3 {
		t.Fatalf("three rooms, one delivery each: %d", hits.Load())
	}
	waitFor(t, "the catch-up to finish", func() bool {
		rig.em.mu.Lock()
		defer rig.em.mu.Unlock()
		return !rig.em.catchup["slack-app"]
	})
}

// I2: a database error while registering is retried, not fatal.
func TestEmitterRegistersThroughABlip(t *testing.T) {
	var hits atomic.Int32
	srv := counting(&hits)
	defer srv.Close()
	rig := newEmitterRig(t, srv.URL, []string{"room.ended"})
	rig.subErrs = 3
	rig.start()
	rig.log("aaaaaaaa", fact(1, factsPhase("Done")))
	rig.waitCursor(t, "aaaaaaaa", 1)
	if hits.Load() != 1 {
		t.Fatalf("%d deliveries", hits.Load())
	}
}

// M-a: a subscriber has at most eight workers at once.
func TestEmitterBoundsWorkers(t *testing.T) {
	var cur, peak atomic.Int32
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := cur.Add(1)
		for {
			if p := peak.Load(); n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		<-release
		cur.Add(-1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	defer close(release)
	rig := newEmitterRig(t, srv.URL, []string{"room.ended"})
	rig.start()
	rooms := make([]string, 20)
	for i := range rooms {
		rooms[i] = "room" + strconv.Itoa(1000+i)
		rig.log(rooms[i], fact(1, factsPhase("Done")))
	}
	waitFor(t, "eight deliveries in flight", func() bool { return peak.Load() == 8 })
	time.Sleep(100 * time.Millisecond) // a ninth, if the bound were missing, would be in by now
	if peak.Load() != 8 {
		t.Fatalf("peak concurrency %d, want 8", peak.Load())
	}
}

// M-b: once the subscriber is disabled, a delivery waiting out its retries stops retrying.
func TestEmitterRetriesStopWhenDisabled(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	rig := newEmitterRig(t, srv.URL, []string{"room.ended"})
	rig.start()
	rig.log("aaaaaaaa", fact(1, factsPhase("Done")))
	waitFor(t, "the first attempt", func() bool { return hits.Load() == 1 })
	rig.em.subs[0].disabled.Store(true) // the worker waits on the fake sleep, which nobody has released
	select {
	case <-rig.slp:
	case <-time.After(5 * time.Second):
		t.Fatal("no retry wait")
	}
	rig.waitCursor(t, "aaaaaaaa", 1)
	if hits.Load() != 1 {
		t.Fatalf("%d attempts after the subscriber was disabled, want 1", hits.Load())
	}
}

// M-c: an unreadable key file is counted no_key on every attempt, the last included.
func TestEmitterNoKeyOnTheLastAttempt(t *testing.T) {
	var hits atomic.Int32
	srv := counting(&hits)
	defer srv.Close()
	rig := newEmitterRig(t, srv.URL, []string{"room.ended"})
	if err := os.Remove(rig.em.Subs[0].SecretFile); err != nil {
		t.Fatal(err)
	}
	rig.start()
	rig.log("aaaaaaaa", fact(1, factsPhase("Done")))
	rig.stepRetries(t)
	rig.waitCursor(t, "aaaaaaaa", 1)
	if rig.outcome("slack-app", "no_key") != 4 || rig.outcome("slack-app", "failed") != 0 || hits.Load() != 0 {
		t.Fatalf("no_key %d, failed %d, hits %d", rig.outcome("slack-app", "no_key"), rig.outcome("slack-app", "failed"), hits.Load())
	}
}

// by is ev written by someone else: the log's trust rests on who wrote an entry.
func by(ev envelope.Event, kind envelope.ActorKind, id string, origin envelope.Origin) envelope.Event {
	ev.Actor = envelope.Actor{Kind: kind, ID: id}
	ev.Origin = origin
	return ev
}

// I-a: a task entry names a repository only from the factory, and the seal ends a room only from
// the broker; any other writer of the room is ignored.
func TestEmitterTrustsOnlyTheFactoryAndTheBroker(t *testing.T) {
	var hits atomic.Int32
	srv := counting(&hits)
	defer srv.Close()
	rig := newEmitterRig(t, srv.URL, []string{"room.ended"})
	rig.withRepo("pppppppp", "")
	rig.withRepo("qqqqqqqq", "")
	rig.start()
	// Another system principal, then a human, write a task fact naming the subscriber's repository.
	rig.log("pppppppp", by(fact(1, factsPR("AwaitingCI", 3)), envelope.ActorSystem, "system:other", envelope.OriginClient))
	rig.log("pppppppp", by(fact(2, factsPhase("Done")), envelope.ActorHuman, "human:mallory", envelope.OriginClient))
	rig.log("pppppppp", seal(3))
	rig.waitCursor(t, "pppppppp", 3)
	if hits.Load() != 0 || rig.outcome("slack-app", "unresolved") != 1 {
		t.Fatalf("%d deliveries, %d unresolved: an untrusted task fact named the repository", hits.Load(), rig.outcome("slack-app", "unresolved"))
	}
	// A human's forged seal does not end the room: it is held, not given up.
	rig.log("qqqqqqqq", fact(1, factsPhase("Done")))
	rig.log("qqqqqqqq", by(seal(2), envelope.ActorHuman, "human:mallory", envelope.OriginClient))
	waitFor(t, "sweeps over the room", func() bool { return rig.asked("qqqqqqqq") >= 3 })
	if rig.cursor("qqqqqqqq") != 0 || rig.outcome("slack-app", "unresolved") != 1 {
		t.Fatal("a seal from a human ended the room")
	}
}

// I-b: the repository compares without case; a log and a Room that really disagree hold the room,
// counted once as a mismatch.
func TestEmitterRepositoryAgreement(t *testing.T) {
	var hits atomic.Int32
	srv := counting(&hits)
	defer srv.Close()
	rig := newEmitterRig(t, srv.URL, []string{"room.ended"})
	rig.withRepo("rrrrrrrr", "SMANA/AI-native-ref") // the Room's spelling differs from the log's and the subscriber's
	rig.withRepo("ssssssss", "Smana/other")
	rig.start()
	f := factsPhase("Done")
	f.PR = &envelope.PRFact{Number: 4, URL: "https://github.com/smana/AI-NATIVE-REF/pull/4"}
	rig.log("rrrrrrrr", fact(1, f))
	rig.waitCursor(t, "rrrrrrrr", 1)
	if hits.Load() != 1 {
		t.Fatalf("a case-only difference is the same repository: %d deliveries", hits.Load())
	}
	rig.log("ssssssss", fact(1, factsPR("Done", 5)))
	waitFor(t, "the mismatch to be counted", func() bool { return rig.outcome("slack-app", "mismatch") == 1 })
	if rig.cursor("ssssssss") != 0 || hits.Load() != 1 {
		t.Fatal("a room whose log and Room disagree was delivered or moved on")
	}
}
