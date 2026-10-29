// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/Smana/maku/api/v1alpha1"
	"github.com/Smana/maku/internal/bridgeapi"
	"github.com/Smana/maku/internal/config"
	"github.com/Smana/maku/internal/envelope"
	"github.com/Smana/maku/internal/humanapi"
	"github.com/Smana/maku/internal/metrics"
	"github.com/Smana/maku/internal/redact"
	"github.com/Smana/maku/internal/runrequest"
	"github.com/Smana/maku/internal/wire"
)

// fakeApproval is one pending approval the fake log holds: answered by a result already
// logged, or past its deadline, or neither; age is how long it has waited.
type fakeApproval struct {
	id            string
	answered, due bool
	age           time.Duration
}

// fakeApprovalLog is an approvals log with state: the sweep closes approvals in it, and a test
// reads what was closed (and why) and what is still open.
type fakeApprovalLog struct {
	mu                      sync.Mutex
	pending                 []fakeApproval
	closed                  map[string]string // approval id -> "superseded" | "expired"
	supersedeErr, expireErr error
	oldestErr               error
}

func (f *fakeApprovalLog) close(reason string, want func(fakeApproval) bool, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err != nil {
		return
	}
	if f.closed == nil {
		f.closed = map[string]string{}
	}
	for _, a := range f.pending {
		if _, done := f.closed[a.id]; !done && want(a) {
			f.closed[a.id] = reason
		}
	}
}

func (f *fakeApprovalLog) SupersedeAnswered(context.Context) ([]envelope.Event, error) {
	f.close("superseded", func(a fakeApproval) bool { return a.answered }, f.supersedeErr)
	return nil, f.supersedeErr
}

func (f *fakeApprovalLog) ExpireDue(context.Context) ([]envelope.Event, error) {
	f.close("expired", func(a fakeApproval) bool { return a.due }, f.expireErr)
	return nil, f.expireErr
}

// OldestPending is the longest wait among the approvals no sweep has closed.
func (f *fakeApprovalLog) OldestPending(context.Context) (time.Duration, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var oldest time.Duration
	n := 0
	for _, a := range f.pending {
		if _, done := f.closed[a.id]; !done {
			n++
			oldest = max(oldest, a.age)
		}
	}
	return oldest, n, f.oldestErr
}

func (f *fakeApprovalLog) closedSet() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return maps.Clone(f.closed)
}

// One sweep: answered approvals are superseded, due ones expired, then the oldest still-pending
// age is exported; a failure does not stop the next step, and a replica that stops leading stops.
func TestSweepApprovals(t *testing.T) {
	quiet := slog.New(slog.DiscardHandler)
	always := func() bool { return true }
	pending := []fakeApproval{
		{id: "answered", answered: true, age: 2 * time.Minute},
		{id: "due", due: true, age: time.Minute},
		{id: "open", age: 90 * time.Second},
	}
	for _, c := range []struct {
		name    string
		l       *fakeApprovalLog
		leading func() bool
		closed  map[string]string
		gauge   []float64
	}{
		{"every step", &fakeApprovalLog{pending: pending}, always,
			map[string]string{"answered": "superseded", "due": "expired"}, []float64{90}},
		{"a failed supersede still expires", &fakeApprovalLog{pending: pending, supersedeErr: errors.New("down")}, always,
			map[string]string{"due": "expired"}, []float64{120}},
		{"a failed expiry still measures", &fakeApprovalLog{pending: pending, expireErr: errors.New("down")}, always,
			map[string]string{"answered": "superseded"}, []float64{90}},
		{"an unreadable age leaves the gauge", &fakeApprovalLog{pending: pending, oldestErr: errors.New("down")}, always,
			map[string]string{"answered": "superseded", "due": "expired"}, nil},
		{"a replica that stops leading stops", &fakeApprovalLog{pending: pending}, func() bool { return false },
			map[string]string{"answered": "superseded"}, nil},
		{"or stops before measuring", &fakeApprovalLog{pending: pending}, onlyOnce(),
			map[string]string{"answered": "superseded", "due": "expired"}, nil},
		// The order is the contract: a call that has its result is answered, whatever its
		// deadline, so supersede must run before expiry or this one would be closed as expired.
		{"answered and past its deadline is superseded, not expired",
			&fakeApprovalLog{pending: []fakeApproval{{id: "both", answered: true, due: true}}}, always,
			map[string]string{"both": "superseded"}, []float64{0}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var gauge []float64
			sweepApprovals(t.Context(), c.l, c.leading, func(_ context.Context, v float64) { gauge = append(gauge, v) }, quiet)
			if got := c.l.closedSet(); !maps.Equal(got, c.closed) {
				t.Fatalf("closed %v, want %v", got, c.closed)
			}
			if !slices.Equal(gauge, c.gauge) {
				t.Fatalf("gauge %v, want %v", gauge, c.gauge)
			}
		})
	}
}

// The sweep runs on the leader only, every 30 s; a replica that stops leading
// exports no stale age.
func TestApprovalLoop(t *testing.T) {
	l := &fakeApprovalLog{pending: []fakeApproval{{id: "open", age: 20 * time.Minute}}}
	tick := make(chan time.Time)
	var mu sync.Mutex
	var gauge []float64
	loop := approvalLoop(l, func(_ context.Context, v float64) { mu.Lock(); gauge = append(gauge, v); mu.Unlock() },
		slog.New(slog.DiscardHandler), func(d time.Duration) (<-chan time.Time, func()) {
			if d != 30*time.Second {
				t.Errorf("period %s", d)
			}
			return tick, func() {}
		})
	if !loop.NeedLeaderElection() {
		t.Fatal("only the leader sweeps")
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- loop.Start(ctx) }()
	tick <- time.Now()
	tick <- time.Now() // the first sweep has finished once the loop takes this one
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(gauge) < 2 || gauge[0] != 1200 || gauge[len(gauge)-1] != 0 {
		t.Fatalf("gauge %v: the age, then 0 once no longer leading", gauge)
	}
}

// The Room's approval policy, for hello and each deadline; an unreadable Room
// is attended.
func TestRoomPolicy(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	room := &v1alpha1.Room{ObjectMeta: metav1.ObjectMeta{Name: "3kq7x2ma", Namespace: "agent-system"},
		Spec: v1alpha1.RoomSpec{Approvals: v1alpha1.Approvals{Profile: "unattended", TTL: "90m", FourEyes: true,
			Overrides: map[string]string{"forge.pr": "human"}}}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(room).Build()
	got := roomPolicy(c, "agent-system")(context.Background(), "3kq7x2ma")
	if got.Profile != "unattended" || got.TTL != "90m" || got.Overrides["forge.pr"] != "human" || len(got.Overrides) != 1 {
		t.Fatalf("policy %+v", got)
	}
	for _, p := range []wire.ApprovalPolicy{roomPolicy(c, "agent-system")(context.Background(), "abcdefgh"), roomPolicy(c, "elsewhere")(context.Background(), "3kq7x2ma")} {
		if p.Profile != "attended" || p.TTL != "" {
			t.Fatalf("an unreadable Room: %+v", p)
		}
	}
}

// blockingReader is a client.Reader whose Get blocks until its ctx ends.
type blockingReader struct{ client.Reader }

func (blockingReader) Get(ctx context.Context, _ client.ObjectKey, _ client.Object, _ ...client.GetOption) error {
	<-ctx.Done()
	return ctx.Err()
}

// The lookup runs under the caller's context: an already-cancelled one returns
// the attended default at once instead of waiting out policyTimeout.
func TestRoomPolicyHonoursTheCallersContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan wire.ApprovalPolicy, 1)
	go func() { done <- roomPolicy(blockingReader{}, "agent-system")(ctx, "3kq7x2ma") }()
	select {
	case p := <-done:
		if p.Profile != "attended" || p.TTL != "" {
			t.Fatalf("policy %+v, want the attended default", p)
		}
	case <-time.After(policyTimeout / 2):
		t.Fatal("the lookup ignored the caller's cancelled context")
	}
}

// maku_rooms_approval_decision_seconds observes each human decision's wait.
func TestDecisionSecondsAreRecorded(t *testing.T) {
	exp, m := newMetrics(t)
	h := humanActorForMetrics(t, m)
	h.OnDecided(90 * time.Second)
	if body := scrape(t, exp.Handler()); !strings.Contains(body, "maku_rooms_approval_decision_seconds_count 1") ||
		!strings.Contains(body, "maku_rooms_approval_decision_seconds_sum 90") {
		t.Fatalf("the decision is not observed:\n%s", body)
	}
}

func humanActorForMetrics(t *testing.T, m *metrics.Set) *humanapi.Actor {
	t.Helper()
	red, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}
	a, err := humanActor(config.HumanConfig{}, &fakeActLog{}, red, fakeRuns{}, fake.NewClientBuilder().Build(), runrequest.Manifest{}, m)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// fakeBrokerLog is the metered store as bridgeAPI sees it: only the sweep's
// methods answer.
type fakeBrokerLog struct {
	bridgeapi.Log
	bridgeapi.Approvals
	*fakeApprovalLog
}

// Review 5.3 M3: :8443 gets the Room's approval policy and the approvals store,
// and the leader gets the approval sweep; a refused Runnable stops start-up.
func TestBridgeAPIIsWired(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	room := &v1alpha1.Room{ObjectMeta: metav1.ObjectMeta{Name: "3kq7x2ma", Namespace: "agent-system"},
		Spec: v1alpha1.RoomSpec{Approvals: v1alpha1.Approvals{Profile: "unattended", TTL: "90m"}}}
	rooms := fake.NewClientBuilder().WithScheme(scheme).WithObjects(room).Build()
	_, m := newMetrics(t)
	red, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}
	l := &fakeBrokerLog{fakeApprovalLog: &fakeApprovalLog{pending: []fakeApproval{
		{id: "answered", answered: true}, {id: "due", due: true}}}}
	var added []manager.Runnable
	add := func(r manager.Runnable) error { added = append(added, r); return nil }
	s, err := bridgeAPI(l, red, nil, nil, nil, rooms, "agent-system", add, m, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if s.RoomPolicy == nil {
		t.Fatal("no RoomPolicy: hello says attended and unattended rooms would wait 30 min")
	}
	if p := s.RoomPolicy(context.Background(), "3kq7x2ma"); p.Profile != "unattended" || p.TTL != "90m" {
		t.Fatal("the Room's approval policy is not wired: unattended rooms would wait 30 min")
	}
	if s.Approvals != l || s.Log != l || s.Redactor != red {
		t.Fatalf("server %+v", s)
	}
	loop, ok := added[0].(*leaderLoop)
	if len(added) != 1 || !ok || loop.every != approvalsEvery {
		t.Fatalf("the approval sweep is not on the leader: %v", added)
	}
	loop.active.Store(true)
	loop.run(t.Context())
	if got := l.closedSet(); !maps.Equal(got, map[string]string{"answered": "superseded", "due": "expired"}) {
		t.Fatalf("the leader closed %v: it does not run the approval sweep", got)
	}
	refuse := func(manager.Runnable) error { return errors.New("manager started") }
	if _, err := bridgeAPI(l, red, nil, nil, nil, rooms, "agent-system", refuse, m, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("a sweep the manager refused must stop start-up")
	}
}
