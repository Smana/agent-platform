// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/bridgeapi"
	"github.com/Smana/agent-platform/internal/config"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/humanapi"
	"github.com/Smana/agent-platform/internal/metrics"
	"github.com/Smana/agent-platform/internal/redact"
	"github.com/Smana/agent-platform/internal/runrequest"
	"github.com/Smana/agent-platform/internal/wire"
)

// fakeApprovalLog records the sweep's calls, in order.
type fakeApprovalLog struct {
	mu                      sync.Mutex
	calls                   []string
	supersedeErr, expireErr error
	oldest                  time.Duration
	oldestErr               error
}

func (f *fakeApprovalLog) record(c string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, c)
}

func (f *fakeApprovalLog) SupersedeAnswered(context.Context) ([]envelope.Event, error) {
	f.record("supersede")
	return nil, f.supersedeErr
}

func (f *fakeApprovalLog) ExpireDue(context.Context) ([]envelope.Event, error) {
	f.record("expire")
	return nil, f.expireErr
}

func (f *fakeApprovalLog) OldestPending(context.Context) (time.Duration, int, error) {
	f.record("oldest")
	return f.oldest, 1, f.oldestErr
}

func (f *fakeApprovalLog) got() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.calls, " ")
}

// One sweep: superseded first, then expired, then the oldest pending age; a
// failure does not stop the next step, and a replica that stops leading stops.
func TestSweepApprovals(t *testing.T) {
	quiet := slog.New(slog.DiscardHandler)
	always := func() bool { return true }
	for _, c := range []struct {
		name    string
		l       *fakeApprovalLog
		leading func() bool
		calls   string
		gauge   []float64
	}{
		{"every step", &fakeApprovalLog{oldest: 90 * time.Second}, always, "supersede expire oldest", []float64{90}},
		{"a failed supersede still expires", &fakeApprovalLog{supersedeErr: errors.New("down")}, always, "supersede expire oldest", []float64{0}},
		{"a failed expiry still measures", &fakeApprovalLog{expireErr: errors.New("down"), oldest: time.Minute}, always, "supersede expire oldest", []float64{60}},
		{"an unreadable age leaves the gauge", &fakeApprovalLog{oldestErr: errors.New("down")}, always, "supersede expire oldest", nil},
		{"a replica that stops leading stops", &fakeApprovalLog{}, func() bool { return false }, "supersede", nil},
		{"or stops before measuring", &fakeApprovalLog{}, onlyOnce(), "supersede expire", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			var gauge []float64
			sweepApprovals(t.Context(), c.l, c.leading, func(_ context.Context, v float64) { gauge = append(gauge, v) }, quiet)
			if got := c.l.got(); got != c.calls {
				t.Fatalf("calls %q, want %q", got, c.calls)
			}
			if len(gauge) != len(c.gauge) || (len(gauge) == 1 && gauge[0] != c.gauge[0]) {
				t.Fatalf("gauge %v, want %v", gauge, c.gauge)
			}
		})
	}
}

// The sweep runs on the leader only, every 30 s; a replica that stops leading
// exports no stale age.
func TestApprovalLoop(t *testing.T) {
	l := &fakeApprovalLog{oldest: 20 * time.Minute}
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
	got := roomPolicy(c, "agent-system")("3kq7x2ma")
	if got.Profile != "unattended" || got.TTL != "90m" || got.Overrides["forge.pr"] != "human" || len(got.Overrides) != 1 {
		t.Fatalf("policy %+v", got)
	}
	for _, p := range []wire.ApprovalPolicy{roomPolicy(c, "agent-system")("abcdefgh"), roomPolicy(c, "elsewhere")("3kq7x2ma")} {
		if p.Profile != "attended" || p.TTL != "" {
			t.Fatalf("an unreadable Room: %+v", p)
		}
	}
}

// rooms_approval_decision_seconds observes each human decision's wait.
func TestDecisionSecondsAreRecorded(t *testing.T) {
	exp, m := newMetrics(t)
	h := humanActorForMetrics(t, m)
	h.OnDecided(90 * time.Second)
	if body := scrape(t, exp.Handler()); !strings.Contains(body, "rooms_approval_decision_seconds_count 1") ||
		!strings.Contains(body, "rooms_approval_decision_seconds_sum 90") {
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
	l := &fakeBrokerLog{fakeApprovalLog: &fakeApprovalLog{}}
	var added []manager.Runnable
	add := func(r manager.Runnable) error { added = append(added, r); return nil }
	s, err := bridgeAPI(l, red, nil, nil, nil, rooms, "agent-system", add, m, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	if s.RoomPolicy == nil {
		t.Fatal("no RoomPolicy: hello says attended and unattended rooms would wait 30 min")
	}
	if p := s.RoomPolicy("3kq7x2ma"); p.Profile != "unattended" || p.TTL != "90m" {
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
	if got := l.got(); got != "supersede expire oldest" {
		t.Fatalf("the leader runs %q, not the approval sweep", got)
	}
	refuse := func(manager.Runnable) error { return errors.New("manager started") }
	if _, err := bridgeAPI(l, red, nil, nil, nil, rooms, "agent-system", refuse, m, slog.New(slog.DiscardHandler)); err == nil {
		t.Fatal("a sweep the manager refused must stop start-up")
	}
}
