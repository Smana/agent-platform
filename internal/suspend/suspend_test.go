// SPDX-License-Identifier: Apache-2.0

package suspend

import (
	"context"
	"errors"
	"maps"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/runwatch"
	"github.com/Smana/agent-platform/internal/store"
)

var (
	now     = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	started = now.Add(-20 * time.Minute)
)

// running is a suspendable run, Running for 20 of its 30 minutes.
func running() runwatch.Run {
	return runwatch.Run{ID: "7f3cq2xz", Room: "3kq7x2ma", Phase: "Running", Suspendable: true, MaxMinutes: 30,
		StartedAt: started, ResourceVersion: "7"}
}

func TestDecide(t *testing.T) {
	after := 5 * time.Minute
	idle := now.Add(-after)
	suspended := func(r runwatch.Run) runwatch.Run { r.Phase, r.OperatingMode = "Suspended", ModeSuspended; return r }
	cases := []struct {
		name      string
		run       func(runwatch.Run) runwatch.Run
		waiting   bool
		idleSince time.Time
		want      map[string]string // nil: no patch
	}{
		{"a run waiting on an idle room is suspended with what it has left", nil, true, idle,
			map[string]string{runwatch.OperatingModeAnnotation: ModeSuspended, runwatch.RemainingSecondsAnnotation: "600"}},
		{"a room idle for less than the threshold waits", nil, true, now.Add(-after + time.Second), nil},
		{"a run that waits on nothing runs on", nil, false, idle, nil},
		{"a run that is not suspendable is never suspended", func(r runwatch.Run) runwatch.Run {
			r.Suspendable = false
			return r
		}, true, idle, nil},
		{"a run not Running yet is never suspended", func(r runwatch.Run) runwatch.Run {
			r.Phase = "Pending"
			return r
		}, true, idle, nil},
		{"a run with no start time is never suspended: its running time is unknown", func(r runwatch.Run) runwatch.Run {
			r.StartedAt = time.Time{}
			return r
		}, true, idle, nil},
		{"a revoked run is left to its revocation", func(r runwatch.Run) runwatch.Run {
			r.Revoked = "manual"
			return r
		}, true, idle, nil},
		{"a suspended run waits while its approval is pending", suspended, true, idle, nil},
		{"a suspended run resumes once nothing is pending", suspended, false, idle,
			map[string]string{runwatch.OperatingModeAnnotation: ModeRunning, runwatch.ResumedAtAnnotation: "2026-10-16T12:00:00Z"}},
		{"a run asked to suspend that has not rendered it yet still resumes", func(r runwatch.Run) runwatch.Run {
			r.OperatingMode = ModeSuspended
			return r
		}, false, idle, map[string]string{runwatch.OperatingModeAnnotation: ModeRunning, runwatch.ResumedAtAnnotation: "2026-10-16T12:00:00Z"}},
		{"a terminal run is never resumed", func(r runwatch.Run) runwatch.Run {
			r = suspended(r)
			r.Phase = "Failed"
			return r
		}, false, idle, nil},
		{"a resumed run counts from its resume, against the seconds it was given", func(r runwatch.Run) runwatch.Run {
			r.RemainingSeconds, r.ResumedAt = "900", now.Add(-10*time.Minute)
			return r
		}, true, idle, map[string]string{runwatch.OperatingModeAnnotation: ModeSuspended, runwatch.RemainingSecondsAnnotation: "300"}},
		{"what is left never drops under a minute", func(r runwatch.Run) runwatch.Run {
			r.StartedAt = now.Add(-40 * time.Minute)
			return r
		}, true, idle, map[string]string{runwatch.OperatingModeAnnotation: ModeSuspended, runwatch.RemainingSecondsAnnotation: "60"}},
		{"seconds above the budget are capped at it", func(r runwatch.Run) runwatch.Run {
			r.RemainingSeconds, r.ResumedAt = "999999", now
			return r
		}, true, idle, map[string]string{runwatch.OperatingModeAnnotation: ModeSuspended, runwatch.RemainingSecondsAnnotation: "1800"}},
		{"seconds the composition ignores count as the full budget", func(r runwatch.Run) runwatch.Run {
			r.RemainingSeconds, r.ResumedAt = "30", now
			return r
		}, true, idle, map[string]string{runwatch.OperatingModeAnnotation: ModeSuspended, runwatch.RemainingSecondsAnnotation: "1800"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := running()
			if tc.run != nil {
				r = tc.run(r)
			}
			got, ok := Decide(after, r, tc.waiting, tc.idleSince, now)
			if ok != (tc.want != nil) || !maps.Equal(got, tc.want) {
				t.Fatalf("got %v %v, want %v", got, ok, tc.want)
			}
		})
	}
}

type fakeApprovals struct {
	open []store.Approval
	err  error
}

func (f fakeApprovals) OpenApprovals(context.Context, string) ([]store.Approval, error) {
	return f.open, f.err
}

func mapper() meta.RESTMapper {
	m := meta.NewDefaultRESTMapper(nil)
	m.Add(runwatch.GVK(), meta.RESTScopeNamespace)
	return m
}

func agentRun(id string, ann map[string]string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(runwatch.GVK())
	u.SetName(runwatch.ClaimPrefix + id)
	u.SetNamespace(runwatch.Namespace)
	u.SetAnnotations(ann)
	_ = unstructured.SetNestedField(u.Object, true, "spec", "suspendable")
	return u
}

func room(profile string) *v1alpha1.Room {
	return &v1alpha1.Room{ObjectMeta: metav1.ObjectMeta{Name: "3kq7x2ma", Namespace: "agent-system"},
		Spec: v1alpha1.RoomSpec{Approvals: v1alpha1.Approvals{Profile: profile}}}
}

func annotations(t *testing.T, c client.Client, id string) map[string]string {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(runwatch.GVK())
	if err := c.Get(t.Context(), types.NamespacedName{Namespace: runwatch.Namespace, Name: runwatch.ClaimPrefix + id}, u); err != nil {
		t.Fatal(err)
	}
	return u.GetAnnotations()
}

// build stores the run's claim and returns the run as the watch would read it, with its version.
func build(t *testing.T, ann map[string]string) (client.Client, runwatch.Run) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).WithRESTMapper(mapper()).
		WithObjects(agentRun("7f3cq2xz", ann)).Build()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(runwatch.GVK())
	if err := c.Get(t.Context(), types.NamespacedName{Namespace: runwatch.Namespace, Name: "xplane-run-7f3cq2xz"}, u); err != nil {
		t.Fatal(err)
	}
	r := running()
	r.ResourceVersion = u.GetResourceVersion()
	return c, r
}

func waitingOn(run string) fakeApprovals {
	return fakeApprovals{open: []store.Approval{{ID: "ap1", RunID: run}}}
}

func TestRoomSuspendsAndResumes(t *testing.T) {
	idle := now.Add(-10 * time.Minute)
	t.Run("an attended room's waiting run gets exactly the two annotations", func(t *testing.T) {
		c, r := build(t, map[string]string{"agents.ogenki.io/usage-tokens": "12"})
		s := &Suspender{Client: c, Approvals: waitingOn("7f3cq2xz"), After: 5 * time.Minute, Now: func() time.Time { return now }}
		if err := s.Room(t.Context(), room("attended"), idle, []runwatch.Run{r}); err != nil {
			t.Fatal(err)
		}
		want := map[string]string{"agents.ogenki.io/usage-tokens": "12", runwatch.OperatingModeAnnotation: ModeSuspended, runwatch.RemainingSecondsAnnotation: "600"}
		if got := annotations(t, c, "7f3cq2xz"); !maps.Equal(got, want) {
			t.Fatalf("annotations %v", got)
		}
	})
	t.Run("the default profile is attended", func(t *testing.T) {
		c, r := build(t, nil)
		s := &Suspender{Client: c, Approvals: waitingOn("7f3cq2xz"), After: 5 * time.Minute, Now: func() time.Time { return now }}
		if err := s.Room(t.Context(), room(""), idle, []runwatch.Run{r}); err != nil {
			t.Fatal(err)
		}
		if got := annotations(t, c, "7f3cq2xz")[runwatch.OperatingModeAnnotation]; got != ModeSuspended {
			t.Fatalf("mode %q", got)
		}
	})
	t.Run("an unattended room is never touched", func(t *testing.T) {
		c, r := build(t, nil)
		s := &Suspender{Client: c, Approvals: waitingOn("7f3cq2xz"), After: 5 * time.Minute, Now: func() time.Time { return now }}
		if err := s.Room(t.Context(), room("unattended"), idle, []runwatch.Run{r}); err != nil {
			t.Fatal(err)
		}
		if got := annotations(t, c, "7f3cq2xz"); len(got) != 0 {
			t.Fatalf("annotations %v", got)
		}
	})
	t.Run("another run's approval does not suspend this one", func(t *testing.T) {
		c, r := build(t, nil)
		s := &Suspender{Client: c, Approvals: waitingOn("aaaaaaaa"), After: 5 * time.Minute, Now: func() time.Time { return now }}
		if err := s.Room(t.Context(), room("attended"), idle, []runwatch.Run{r}); err != nil {
			t.Fatal(err)
		}
		if got := annotations(t, c, "7f3cq2xz"); len(got) != 0 {
			t.Fatalf("annotations %v", got)
		}
	})
	t.Run("a decided approval resumes the run", func(t *testing.T) {
		c, r := build(t, map[string]string{runwatch.OperatingModeAnnotation: ModeSuspended, runwatch.RemainingSecondsAnnotation: "600"})
		r.Phase, r.OperatingMode, r.RemainingSeconds = "Suspended", ModeSuspended, "600"
		s := &Suspender{Client: c, Approvals: fakeApprovals{}, After: 5 * time.Minute, Now: func() time.Time { return now }}
		if err := s.Room(t.Context(), room("attended"), idle, []runwatch.Run{r}); err != nil {
			t.Fatal(err)
		}
		want := map[string]string{runwatch.OperatingModeAnnotation: ModeRunning, runwatch.RemainingSecondsAnnotation: "600", runwatch.ResumedAtAnnotation: "2026-10-16T12:00:00Z"}
		if got := annotations(t, c, "7f3cq2xz"); !maps.Equal(got, want) {
			t.Fatalf("annotations %v", got)
		}
	})
	t.Run("a stale read writes nothing", func(t *testing.T) {
		c, r := build(t, nil)
		r.ResourceVersion = "1"
		s := &Suspender{Client: c, Approvals: waitingOn("7f3cq2xz"), After: 5 * time.Minute, Now: func() time.Time { return now }}
		if err := s.Room(t.Context(), room("attended"), idle, []runwatch.Run{r}); !apierrors.IsConflict(err) {
			t.Fatalf("a run changed since the watch read it is decided again at the next resync: %v", err)
		}
		if got := annotations(t, c, "7f3cq2xz"); len(got) != 0 {
			t.Fatalf("annotations %v", got)
		}
	})
	t.Run("an unreadable approval list touches nothing", func(t *testing.T) {
		c, r := build(t, nil)
		boom := errors.New("down")
		s := &Suspender{Client: c, Approvals: fakeApprovals{err: boom}, After: 5 * time.Minute, Now: func() time.Time { return now }}
		if err := s.Room(t.Context(), room("attended"), idle, []runwatch.Run{r}); !errors.Is(err, boom) {
			t.Fatalf("%v", err)
		}
		if got := annotations(t, c, "7f3cq2xz"); len(got) != 0 {
			t.Fatalf("annotations %v", got)
		}
	})
}
