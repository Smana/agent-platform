// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache/informertest"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllertest"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/runwatch"
)

// fakeRunLog is the run events' store: idempotent by key, safe across the
// informer's and the leader's goroutines.
type fakeRunLog struct {
	mu   sync.Mutex
	keys map[string]string // origin client/seq -> payload
}

func (f *fakeRunLog) Append(_ context.Context, d envelope.Draft) (envelope.Event, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	k := fmt.Sprintf("%s/%d", d.OriginClient, d.OriginSeq)
	if _, dup := f.keys[k]; dup {
		return envelope.Event{}, true, nil
	}
	f.keys[k] = string(d.Payload)
	return envelope.Event{}, false, nil
}

func (f *fakeRunLog) LastHarnessStatus(context.Context, string, string) (string, error) {
	return "", nil
}

func (f *fakeRunLog) snapshot() map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]string, len(f.keys))
	for k, v := range f.keys {
		out[k] = v
	}
	return out
}

// fakeOpen is the sweep's query; it signals every call.
type fakeOpen struct{ swept chan struct{} }

func (f fakeOpen) Unfinished(context.Context, string, int64, int64) ([]envelope.Event, error) {
	f.swept <- struct{}{}
	return nil, nil
}

func runClaim(id, room, phase string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "xplane-run-" + id, "namespace": runwatch.Namespace},
		"spec":     map[string]any{"roomRef": room, "role": "implementer"},
		"status":   map[string]any{"phase": phase},
	}}
	u.SetGroupVersionKind(runwatch.GVK())
	return u
}

// Review I1: the leader sweeps, and /readyz admits, only once the watch's own
// handler holds the informer's first list, not when the cache says it synced.
func TestWireRuns(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	inf := controllertest.NewFakeInformer() // not synced: its first list is still arriving
	informers := &informertest.FakeInformers{
		InformersByGVK: map[schema.GroupVersionKind]toolscache.SharedIndexInformer{runwatch.GVK(): inf}}
	store := &fakeRunLog{keys: map[string]string{}}
	open := fakeOpen{swept: make(chan struct{}, 10)}
	var runnables []manager.Runnable
	add := func(r manager.Runnable) error { runnables = append(runnables, r); return nil }
	rw, err := wireRuns(ctx, informers, add, slog.New(slog.DiscardHandler), open, &runwatch.Events{Store: store},
		func(time.Duration) (<-chan time.Time, func()) { return nil, func() {} })
	if err != nil {
		t.Fatal(err)
	}
	if len(runnables) != 1 {
		t.Fatalf("%d runnables added, want the leader", len(runnables))
	}
	if le, ok := runnables[0].(manager.LeaderElectionRunnable); !ok || !le.NeedLeaderElection() {
		t.Fatal("the run events' Runnable must run on the leader only")
	}

	inf.Add(runClaim("7f3cq2xz", "3kq7x2ma", "Running"))
	if _, live := rw.watch.Live("7f3cq2xz"); !live {
		t.Fatal("every replica mirrors a run for bridge admission")
	}
	if got := store.snapshot(); len(got) != 0 {
		t.Fatalf("a follower appended %v", got)
	}
	if rw.synced(expired(ctx)) {
		t.Fatal("the watch reports synced before its handler has the first list")
	}

	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- runnables[0].Start(runCtx) }()
	select {
	case <-open.swept:
		t.Fatal("the sweep ran before the watch's handler synced: it would end every run it has not seen yet")
	case <-time.After(300 * time.Millisecond):
	}

	inf.Synced()
	select {
	case <-open.swept:
	case <-ctx.Done():
		t.Fatal("the sweep never ran once the watch synced")
	}
	if !rw.synced(expired(ctx)) {
		t.Fatal("a synced watch reports not synced")
	}
	if got := store.snapshot(); got["broker:run:7f3cq2xz/1"] == "" || got["broker:run:7f3cq2xz/2"] == "" {
		t.Fatalf("the new leader's replay did not record the run's join and start: %v", got)
	}

	inf.Delete(runClaim("7f3cq2xz", "3kq7x2ma", "Running"))
	if end := store.snapshot()["broker:run:7f3cq2xz/3"]; !strings.Contains(end, `"reason":"deleted"`) {
		t.Fatalf("a claim deleted mid-run ends as %q, want deleted", end)
	}
	stop()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("the leader did not stop with its context")
	}

	if _, err := wireRuns(ctx, &informertest.FakeInformers{Error: errors.New("boom")}, add, slog.New(slog.DiscardHandler),
		open, &runwatch.Events{Store: store}, nil); err == nil {
		t.Fatal("an informer that cannot be had fails startup")
	}
	if _, err := wireRuns(ctx, &informertest.FakeInformers{}, func(manager.Runnable) error { return errors.New("started") },
		slog.New(slog.DiscardHandler), open, &runwatch.Events{Store: store}, nil); err == nil {
		t.Fatal("a leader the manager refuses fails startup")
	}
}

// expired is a context already ended: synced answers from what it has, at once.
func expired(ctx context.Context) context.Context {
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	return ctx
}

func TestManagerOptions(t *testing.T) {
	for _, ns := range []string{"agent-system", "rooms-test"} {
		t.Run(ns, func(t *testing.T) {
			o := managerOptions(ns)
			if !o.LeaderElection || o.LeaderElectionID != "room-broker" || o.LeaderElectionNamespace != ns ||
				!o.LeaderElectionReleaseOnCancel {
				t.Fatalf("leader election: %+v", o)
			}
			if o.GracefulShutdownTimeout == nil || *o.GracefulShutdownTimeout != managerDrain {
				t.Fatalf("the manager's drain must fit the pod's grace: %v", o.GracefulShutdownTimeout)
			}
			if o.Metrics.BindAddress != "0" || o.HealthProbeBindAddress != "0" {
				t.Fatal(":9090 serves metrics and probes; the manager's own listeners stay off")
			}
			if o.Client.Cache == nil || o.Client.Cache.Unstructured {
				t.Fatal("unstructured reads (the finalizer's run list) must reach the API server")
			}
			// RBAC grants exactly these namespaces: a cluster-wide informer is refused.
			want := map[string][]string{"Room": {ns}, runwatch.GVK().Kind: {runwatch.Namespace}}
			if len(o.Cache.ByObject) != len(want) || o.Cache.DefaultNamespaces != nil {
				t.Fatalf("cached kinds: %v, default namespaces %v", o.Cache.ByObject, o.Cache.DefaultNamespaces)
			}
			for obj, by := range o.Cache.ByObject {
				kind := "Room"
				if u, ok := obj.(*unstructured.Unstructured); ok {
					kind = u.GroupVersionKind().Kind
					if u.GroupVersionKind() != runwatch.GVK() {
						t.Fatalf("watches %v, want the AgentRun GVK", u.GroupVersionKind())
					}
				} else if _, ok := obj.(*v1alpha1.Room); !ok {
					t.Fatalf("caches %T", obj)
				}
				var got []string
				for n := range by.Namespaces {
					got = append(got, n)
				}
				if !slices.Equal(got, want[kind]) {
					t.Fatalf("%s cached in %v, want %v", kind, got, want[kind])
				}
			}
		})
	}
}

type fakeRefresher struct {
	calls atomic.Int32
	err   error
}

func (f *fakeRefresher) Refresh(ctx context.Context) error {
	f.calls.Add(1)
	if _, ok := ctx.Deadline(); !ok {
		return errors.New("a refresh must be bounded")
	}
	return f.err
}

// Ruling AQ: every replica refreshes each issuer at start, then about hourly,
// whatever fails; the start is a lazy verifier's first fetch.
func TestRefreshJWKS(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	ok, failing := &fakeRefresher{}, &fakeRefresher{err: errors.New("issuer down")}
	waits, tick := make(chan time.Duration, 1), make(chan time.Time)
	after := func(d time.Duration) <-chan time.Time { waits <- d; return tick }
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- refreshJWKS(runCtx, []refresher{failing, ok}, after) }()
	for round := range int32(3) {
		d := <-waits // the loop waits: every verifier has had round+1 refreshes
		if d < jwksRefreshEvery || d >= jwksRefreshEvery+jwksJitter {
			t.Fatalf("waits %s, want an hour plus under %s of jitter", d, jwksJitter)
		}
		if ok.calls.Load() != round+1 || failing.calls.Load() != round+1 {
			t.Fatalf("round %d: refreshed %d and %d times; a failure must not skip the others",
				round, failing.calls.Load(), ok.calls.Load())
		}
		if round < 2 {
			tick <- time.Now()
		}
	}
	stop()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("refreshJWKS did not stop with its context")
	}
}
