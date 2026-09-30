// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"errors"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	roomv1 "github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/forge"
)

type flakyForge struct {
	*forge.Fake
	down bool
}

func (f *flakyForge) Comment(ctx context.Context, n int, body string) error {
	if f.down {
		return errors.New("github down")
	}
	return f.Fake.Comment(ctx, n, body)
}

// Probe 2: GitHub is down when the task ends; the end is never narrated afterwards.
func TestProbeTheEndIsNarratedAfterAnOutage(t *testing.T) {
	g := newRig(t, issueTask("3buqdlot", 7, "x"))
	ff := &flakyForge{Fake: g.f}
	g.r.Forge = ff
	tk := g.reconcile(t, "3buqdlot", 3)
	tk.Annotations = map[string]string{v1alpha1.AnnotationStop: "true"}
	_ = g.c.Update(t.Context(), tk)
	ff.down = true
	_, err := g.r.Reconcile(t.Context(), reqFor("3buqdlot"))
	t.Logf("reconcile during outage: %v", err)
	ff.down = false
	for range 3 {
		_, _ = g.r.Reconcile(t.Context(), reqFor("3buqdlot"))
	}
	var got v1alpha1.Task
	_ = g.c.Get(t.Context(), keyFor("3buqdlot"), &got)
	t.Logf("phase=%s narrated=%v comments=%d", got.Status.Phase, got.Status.Narrated, len(g.f.Comments(7)))
	if len(g.f.Comments(7)) < 2 {
		t.Errorf("DEFECT: the Stopped end was never narrated after GitHub came back")
	}
}

// landedForge posts the comment and then reports an error, once: the double failure SO names.
type landedForge struct {
	*forge.Fake
	lie bool
}

func (f *landedForge) Comment(ctx context.Context, n int, body string) error {
	if err := f.Fake.Comment(ctx, n, body); err != nil {
		return err
	}
	if f.lie {
		f.lie = false
		return errors.New("502 after the write")
	}
	return nil
}

// Ruling SO: the end is in the outbox with the phase; GitHub down, then back, gives exactly one
// end comment, and the drained task is left alone.
func TestTheEndIsPostedOnceAfterAnOutage(t *testing.T) {
	g := newRig(t, issueTask("3buqdlot", 7, "x"))
	ff := &flakyForge{Fake: g.f}
	g.r.Forge = ff
	tk := g.reconcile(t, "3buqdlot", 3)
	tk.Annotations = map[string]string{v1alpha1.AnnotationStop: "true"}
	_ = g.c.Update(t.Context(), tk)
	ff.down = true
	if _, err := g.r.Reconcile(t.Context(), reqFor("3buqdlot")); err == nil {
		t.Fatal("an undrained outbox retries")
	}
	tk = g.reconcile(t, "3buqdlot", 0)
	if tk.Status.Phase != v1alpha1.PhaseStopped || len(tk.Status.Outbox) != 1 || !strings.HasPrefix(tk.Status.Outbox[0].Key, "end-stopped") {
		t.Fatalf("the phase and its narration are written together: %s %+v", tk.Status.Phase, tk.Status.Outbox)
	}
	ff.down = false
	for range 3 {
		res, err := g.r.Reconcile(t.Context(), reqFor("3buqdlot"))
		if err != nil || res.RequeueAfter != 0 {
			t.Fatalf("%v %v", err, res)
		}
	}
	tk = g.reconcile(t, "3buqdlot", 0)
	if n := strings.Count(strings.Join(g.f.Comments(7), "\n"), "was stopped"); n != 1 || len(tk.Status.Outbox) != 0 {
		t.Fatalf("%d end comments, outbox %+v", n, tk.Status.Outbox)
	}
	if strings.Join(g.metrics.recorded, "|") != "intervention stop|task_tokens standard solo review" {
		t.Fatalf("an ended task is drained, never stepped again: %q", g.metrics.recorded)
	}
}

// A post that landed but answered an error is found by its marker on the retry, not repeated.
func TestALandedPostIsNotRepeated(t *testing.T) {
	g := newRig(t, issueTask("3buqdlot", 7, "x"))
	lf := &landedForge{Fake: g.f, lie: true}
	g.r.Forge = lf
	if _, err := g.r.Reconcile(t.Context(), reqFor("3buqdlot")); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		_, _ = g.r.Reconcile(t.Context(), reqFor("3buqdlot"))
	}
	if tk := g.reconcile(t, "3buqdlot", 2); tk.Status.Phase != v1alpha1.PhaseImplementing || len(tk.Status.Outbox) != 0 {
		t.Fatalf("%s %+v", tk.Status.Phase, tk.Status.Outbox)
	}
	if c := g.f.Comments(7); len(c) != 1 || !strings.Contains(c[0], "started run") {
		t.Fatalf("%q", c)
	}
}

// The started narration survives an outage at the run's start, and the run is recorded meanwhile.
func TestTheStartIsPostedAfterAnOutage(t *testing.T) {
	g := newRig(t, issueTask("3buqdlot", 7, "x"))
	ff := &flakyForge{Fake: g.f, down: true}
	g.r.Forge = ff
	g.reconcile(t, "3buqdlot", 2)
	_, _ = g.r.Reconcile(t.Context(), reqFor("3buqdlot"))
	tk := g.reconcile(t, "3buqdlot", 0)
	if tk.Status.Phase != v1alpha1.PhaseImplementing || len(tk.Status.Runs) != 1 || len(tk.Status.Outbox) != 1 {
		t.Fatalf("%s %+v", tk.Status.Phase, tk.Status.Outbox)
	}
	ff.down = false
	if tk := g.reconcile(t, "3buqdlot", 1); len(tk.Status.Outbox) != 0 || len(g.f.Comments(7)) != 1 || len(g.runs.specs) != 1 {
		t.Fatalf("%+v %q", tk.Status.Outbox, g.f.Comments(7))
	}
}

// Review M-a: a narration is posted only once the status write that queued it has succeeded.
// Here the write recording "ended, no PR" conflicts; the replay finds the PR the agent opened
// meanwhile, and the issue never hears "no pull request".
func TestNothingIsPostedBeforeItsTransitionIsWritten(t *testing.T) {
	conflict := false
	c := fake.NewClientBuilder().WithScheme(scheme()).WithStatusSubresource(&v1alpha1.Task{}, &roomv1.Room{}).
		WithObjects(issueTask("3buqdlot", 7, "x")).
		WithInterceptorFuncs(interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, cl client.Client, sub string, o client.Object, opts ...client.SubResourceUpdateOption) error {
			if conflict {
				conflict = false
				return apierrors.NewConflict(schema.GroupResource{Resource: "tasks"}, o.GetName(), errors.New("stale"))
			}
			return cl.SubResource(sub).Update(ctx, o, opts...)
		}}).Build()
	g := newRig(t)
	g.c, g.r.Client = c, c
	g.reconcile(t, "3buqdlot", 3)
	g.runs.set("7f3cq2xz", "Succeeded")
	g.log.end("7f3cq2xz", "Succeeded", "agent_finished")
	conflict = true
	if _, err := g.r.Reconcile(t.Context(), reqFor("3buqdlot")); err == nil {
		t.Fatal("the conflict is returned")
	}
	if c := g.f.Comments(7); len(c) != 1 {
		t.Fatalf("posted before its transition was written: %q", c)
	}
	g.f.SetBranch("agent/3buqdlot", 12)
	g.f.SetPR(forge.PR{Number: 12, URL: "https://github.com/Smana/cloud-native-ref/pull/12", State: "OPEN"})
	tk := g.reconcile(t, "3buqdlot", 1)
	if all := strings.Join(g.f.Comments(7), "\n"); tk.Status.Phase != v1alpha1.PhaseAwaitingHuman ||
		strings.Contains(all, "no pull request") || !strings.Contains(all, "#12") {
		t.Fatalf("%s %q", tk.Status.Phase, g.f.Comments(7))
	}
}
