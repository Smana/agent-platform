// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/metrics"
	"github.com/Smana/agent-platform/internal/verdictpost"
)

// The loop runs its job on every tick while it leads, and says so: the poster
// posts only while Start runs (ruling SY).
func TestLeaderLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	tick := make(chan time.Time)
	var active atomic.Bool
	ran := make(chan bool, 4)
	l := &leaderLoop{every: time.Hour, active: &active, run: func(context.Context) { ran <- active.Load() },
		ticker: func(time.Duration) (<-chan time.Time, func()) { return tick, func() {} }}
	if !l.NeedLeaderElection() {
		t.Fatal("the leader runs it, and only the leader")
	}
	done := make(chan error, 1)
	go func() { done <- l.Start(ctx) }()
	for range 2 {
		tick <- time.Now()
		if !<-ran {
			t.Fatal("ran while not marked as leading")
		}
	}
	cancel()
	if err := <-done; err != nil || active.Load() {
		t.Fatalf("err %v, active after the lease: %v", err, active.Load())
	}
}

type fakeVerdictLog struct{ drafts []envelope.Draft }

func (f *fakeVerdictLog) UnpostedVerdicts(context.Context, time.Time, int) ([]envelope.Event, error) {
	return nil, nil
}

func (f *fakeVerdictLog) Append(_ context.Context, d envelope.Draft) (envelope.Event, bool, error) {
	f.drafts = append(f.drafts, d)
	return envelope.Event{}, false, nil
}

func TestVerdictPosterWiring(t *testing.T) {
	var added []manager.Runnable
	add := func(r manager.Runnable) error { added = append(added, r); return nil }
	m, _ := metrics.New(nil)
	log := slog.New(slog.DiscardHandler)
	if err := addVerdictPoster("", add, &fakeVerdictLog{}, fake.NewClientBuilder().Build(), "agent-system", "https://rooms", m, log); err != nil || len(added) != 0 {
		t.Fatalf("ROOMS_GITHUB_APP_DIR unset: no poster, %d added, %v", len(added), err)
	}
	if err := addVerdictPoster(t.TempDir(), add, &fakeVerdictLog{}, fake.NewClientBuilder().Build(), "agent-system", "https://rooms", m, log); err != nil || len(added) != 1 {
		t.Fatalf("%d added, %v", len(added), err)
	}
	if le, ok := added[0].(interface{ NeedLeaderElection() bool }); !ok || !le.NeedLeaderElection() {
		t.Fatal("the poster runs on the leader only")
	}
}

// A room's data class comes from its Room; one that cannot be read is never public.
func TestRoomDataClass(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	room := &v1alpha1.Room{ObjectMeta: metav1.ObjectMeta{Name: "3kq7x2ma", Namespace: "agent-system"}, Spec: v1alpha1.RoomSpec{DataClass: "public"}}
	dc := roomDataClass(fake.NewClientBuilder().WithScheme(scheme).WithObjects(room).Build(), "agent-system")
	if got := dc(t.Context(), "3kq7x2ma"); got != "public" {
		t.Fatalf("data class %q", got)
	}
	if got := dc(t.Context(), "aaaaaaaa"); got != "" {
		t.Fatalf("a missing Room: %q", got)
	}
	if got := roomDataClass(fake.NewClientBuilder().WithScheme(scheme).WithObjects(room).Build(), "elsewhere")(t.Context(), "3kq7x2ma"); got != "" {
		t.Fatalf("another namespace's Room: %q", got)
	}
}

// Every result reaches rooms_verdict_posts_total{result}.
func TestVerdictResultsAreCounted(t *testing.T) {
	exp, err := metrics.NewExporter("test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = exp.Shutdown(t.Context()) }()
	m, err := metrics.New(exp.Meter())
	if err != nil {
		t.Fatal(err)
	}
	count := verdictResults(m)
	count(t.Context(), "posted")
	count(t.Context(), "error")
	rec := httptest.NewRecorder()
	exp.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	body, _ := io.ReadAll(rec.Body)
	for _, want := range []string{`rooms_verdict_posts_total{result="posted"} 1`, `rooms_verdict_posts_total{result="error"} 1`} {
		if !strings.Contains(string(body), want+"\n") {
			t.Errorf("missing %s", want)
		}
	}
}

// leadingProbe records whether the poster held the lease when it ran.
type leadingProbe struct {
	p    *verdictpost.Poster
	seen chan bool
}

func (l *leadingProbe) Enabled() bool { l.seen <- l.p.Leading(); return false }
func (*leadingProbe) Comment(context.Context, string, string, string) (string, error) {
	return "", nil
}

// The poster's Leading is its loop's lease: false before and after Start,
// true while the loop runs it (ruling SY).
func TestThePosterLeadsWithItsLoop(t *testing.T) {
	probe := &leadingProbe{seen: make(chan bool, 1)}
	p := &verdictpost.Poster{Now: time.Now}
	probe.p, p.GitHub = p, probe
	tick := make(chan time.Time)
	l := posterLoop(p, slog.New(slog.DiscardHandler), func(time.Duration) (<-chan time.Time, func()) { return tick, func() {} })
	if p.Leading() {
		t.Fatal("leading before the lease")
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- l.Start(ctx) }()
	tick <- time.Now()
	if !<-probe.seen {
		t.Fatal("the poster ran without the lease")
	}
	cancel()
	<-done
	if p.Leading() {
		t.Fatal("leading after the lease")
	}
}
