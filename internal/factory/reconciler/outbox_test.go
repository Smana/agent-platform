// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"errors"
	"strings"
	"testing"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
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
