// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/bridgeapi"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/redact"
	"github.com/Smana/agent-platform/internal/runwatch"
	"github.com/Smana/agent-platform/internal/store"
	"github.com/Smana/agent-platform/internal/wire"
)

const leaseRoom = "3kq7x2ma"

// fakeClock is the time both the bridge and the lease read; the test moves it.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// leaseLog is bridgeapi.Log with the store's lease semantics on a fake clock: a
// holder keeps the room while it is live, however long since it was seen, and
// loses it only once its run ended (store.ClaimBridge, ruling SBB).
type leaseLog struct {
	clock *fakeClock

	mu      sync.Mutex
	holder  string
	seenAt  time.Time
	claims  map[string]int // hello claims per run
	appends map[string]int // bridge appends per run
}

func (l *leaseLog) Append(_ context.Context, d envelope.Draft) (envelope.Event, bool, error) {
	return envelope.Event{RoomID: d.RoomID, Type: d.Type}, false, nil
}

func (l *leaseLog) AppendAsBridge(_ context.Context, run string, d envelope.Draft) (envelope.Event, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.holder != run {
		return envelope.Event{}, false, store.ErrLeaseLost
	}
	l.appends[run]++
	return envelope.Event{RoomID: d.RoomID, Type: d.Type}, false, nil
}

func (l *leaseLog) Range(context.Context, string, int64, int) ([]envelope.Event, error) {
	return nil, nil
}

func (l *leaseLog) Cursor(context.Context, string, string) (int64, error) { return 0, nil }

func (l *leaseLog) Deliveries(context.Context, string, string, int64, int64, int) ([]envelope.Event, error) {
	return nil, nil
}

func (l *leaseLog) Room(_ context.Context, id string) (store.RoomState, error) {
	return store.RoomState{ID: id}, nil
}

func (l *leaseLog) ClaimBridge(ctx context.Context, _, run string, live func(context.Context, string) bool) (string, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.claims[run]++
	if l.holder != "" && l.holder != run && live(ctx, l.holder) {
		return l.holder, false, nil
	}
	l.holder, l.seenAt = run, l.clock.now()
	return run, true, nil
}

func (l *leaseLog) TouchBridge(_ context.Context, _, run string) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.holder != run {
		return false, nil
	}
	l.seenAt = l.clock.now()
	return true, nil
}

func (l *leaseLog) read(f func(l *leaseLog) int) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return f(l)
}

// runTokens maps "Bearer <runId>" to that run's principal.
type runTokens struct{}

func (runTokens) Authenticate(r *http.Request) (authn.Principal, error) {
	id, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || id == "" {
		return authn.Principal{}, authn.ErrUnauthenticated
	}
	return authn.Principal{Kind: envelope.ActorAgent, ID: "agent:" + id, RunID: id}, nil
}

// liveRuns holds every run live, in leaseRoom.
type liveRuns struct{}

func (liveRuns) Live(id string) (runwatch.Run, bool) {
	return runwatch.Run{ID: id, Room: leaseRoom, Role: "implementer", Phase: "Running"}, true
}

// leaseRig is a real bridge for run "runa0001" pushing to the real broker API
// over TLS, both on one fake clock, with a quiet harness.
type leaseRig struct {
	ctx     context.Context // bounds every wait, so a regression fails fast
	clock   *fakeClock
	log     *leaseLog
	harness *fakeAgentServer
	srv     *httptest.Server
	logs    *lockedBuffer
}

const (
	leaseRunA = "runa0001"
	leaseRunB = "runb0002"
)

func newLeaseRig(t *testing.T) *leaseRig {
	t.Helper()
	clock := &fakeClock{t: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)}
	ll := &leaseLog{clock: clock, claims: map[string]int{}, appends: map[string]int{}}
	red, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}
	api := &bridgeapi.Server{Log: ll, Redactor: red, Runs: runTokens{}, Systems: runTokens{}, Watch: liveRuns{}}
	srv := httptest.NewTLSServer(api.Routes())
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	br, err := NewBroker(srv.URL, writeToken(t, dir, leaseRunA), ca)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeAgentServer{pageSize: 100, status: "running"}
	logs := &lockedBuffer{}
	b := &Bridge{Harness: NewHarness(f.start(t, conv).URL, conv), Broker: br, RunID: leaseRunA,
		Interval: 5 * time.Millisecond, MinBackoff: time.Millisecond, MaxBackoff: 20 * time.Millisecond,
		FlushGrace: time.Second, Now: clock.now, Logger: slog.New(slog.NewJSONHandler(logs, nil))}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = b.Run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	r := &leaseRig{ctx: ctx, clock: clock, log: ll, harness: f, srv: srv, logs: logs}
	eventually(ctx, t, "run A holds the room", func() bool {
		return ll.read(func(l *leaseLog) int { return l.claims[leaseRunA] }) == 1 && f.searched() > 0
	})
	return r
}

// quiet moves the clock by d, then waits for one whole bridge step after the
// move: two more harness polls mean a step that began after it has ended,
// its flush included.
func (r *leaseRig) quiet(t *testing.T, d time.Duration) {
	t.Helper()
	r.clock.add(d)
	start := r.harness.searched()
	eventually(r.ctx, t, "a step runs after the clock moves", func() bool { return r.harness.searched() >= start+2 })
}

// hello is run's hello to the broker, as its bridge would send it.
func (r *leaseRig) hello(t *testing.T, run string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(r.ctx, http.MethodPost, r.srv.URL+"/v1/bridge/hello", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+run)
	resp, err := r.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	var e wire.Error
	_ = json.NewDecoder(resp.Body).Decode(&e)
	return resp.StatusCode, e.Reason
}

// Review I2 (Ruling AX, SBB): a quiet run (a long LLM call, a pending
// confirmation) pushes no items, yet keeps its room against a second run for as
// long as it is live, and its next event still lands.
func TestAQuietRunKeepsItsRoom(t *testing.T) {
	r := newLeaseRig(t)
	for range 5 { // 150 s of silence, past the broker's 2 min window
		r.quiet(t, 30*time.Second)
	}
	if code, reason := r.hello(t, leaseRunB); code != http.StatusConflict || reason != wire.ReasonRoomBusy {
		t.Errorf("a second run's hello after 150 s of quiet = %d %s, want 409 %s", code, reason, wire.ReasonRoomBusy)
	}
	before := r.log.read(func(l *leaseLog) int { return l.appends[leaseRunA] })
	r.harness.add(chatEvent("back from a long LLM call"))
	eventually(r.ctx, t, "run A's next event is appended", func() bool {
		return r.log.read(func(l *leaseLog) int { return l.appends[leaseRunA] }) > before
	})
}

// The heartbeat is fenced like an append: a lease another run took surfaces as
// lease_lost, and the bridge stops and says hello again.
func TestAHeartbeatThatFindsTheLeaseGoneStops(t *testing.T) {
	r := newLeaseRig(t)
	r.log.mu.Lock()
	r.log.holder, r.log.seenAt = leaseRunB, r.clock.now().Add(time.Hour) // B holds a fresh lease
	r.log.mu.Unlock()
	r.quiet(t, 30*time.Second)
	r.quiet(t, time.Second) // past the backoff the 409 set
	eventually(r.ctx, t, "the bridge says hello again", func() bool {
		return r.log.read(func(l *leaseLog) int { return l.claims[leaseRunA] }) >= 2
	})
	if !strings.Contains(r.logs.String(), wire.ReasonLeaseLost) {
		t.Fatalf("the heartbeat's 409 was not handled as a lost lease:\n%s", r.logs.String())
	}
}
