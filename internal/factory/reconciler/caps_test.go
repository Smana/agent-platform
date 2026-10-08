// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/runs"
)

func TestQueuesAndReviewerTier(t *testing.T) {
	g := pairRig(t)
	var tk v1alpha1.Task
	_ = g.c.Get(t.Context(), client.ObjectKey{Namespace: "agent-system", Name: "3buqdlot"}, &tk)
	if got := g.runs.specs[rid(0)].Queue; got != runs.QueueFactory {
		t.Fatalf("factory runs are admitted by Kueue's factory queue: %q", got)
	}
	s := g.runs.specs[rid(1)]
	if s.Model != g.r.Cfg.Tiers["frontier"].Model || s.MaxTokens != tk.Spec.Budget.RunTokens {
		t.Fatalf("the reviewer uses another tier's model and the task's run budget: %+v", s)
	}
}

func TestWIPHoldsReviewClassWork(t *testing.T) {
	var waiting []client.Object
	for _, n := range []string{"aaaaaaaa", "bbbbbbbb"} {
		o := issueTask(n, 1, "x")
		o.Status.Phase = v1alpha1.PhaseAwaitingHuman
		waiting = append(waiting, o)
	}
	g := newRig(t, append(waiting, issueTask("3buqdlot", 7, "x"))...)
	g.r.Cfg.Caps.AwaitingHumanWIP = 2
	tk := g.reconcile(t, "3buqdlot", 3)
	if tk.Status.Phase != v1alpha1.PhaseQueued || tk.Status.Reason != "waiting_review_wip" {
		t.Fatalf("%s %s", tk.Status.Phase, tk.Status.Reason)
	}
}

func TestTaskTokenCap(t *testing.T) {
	over := func() *v1alpha1.Task {
		tk := awaiting()
		tk.Status.Phase, tk.Status.NextTrigger = v1alpha1.PhaseQueued, "human"
		tk.Spec.Budget.TaskTokens = 3_000_000
		tk.Status.Usage.Tokens = 3_100_000
		// observe rebuilds the total from the records (R49); a record keeps its last reading
		// when its claim is gone, which is the shape of a settled, over-cap task here.
		tk.Status.Runs[0].Tokens = 3_100_000
		return tk
	}
	// The task has a pull request, so Queued re-reads it (lateReviews) before the revision starts.
	g := newRig(t, over(), roomOf("3buqdlot"))
	g.f.SetPR(pr12())
	got := g.reconcile(t, "3buqdlot", 1)
	if got.Status.Phase != v1alpha1.PhaseImplementing {
		t.Fatal("in shadow the cap is counted, not enforced (R3)")
	}
	g = newRig(t, over(), roomOf("3buqdlot"))
	g.f.SetPR(pr12())
	g.r.Cfg.Budgets.EnforceTask = true
	got = g.reconcile(t, "3buqdlot", 1)
	if got.Status.Phase != v1alpha1.PhaseEscalated || got.Status.Reason != "budget-task" {
		t.Fatalf("%s %s", got.Status.Phase, got.Status.Reason)
	}
}

// R34: the factory's own day is checked before every run it starts, not only by the meter.
func TestFactoryDailyBudget(t *testing.T) {
	spent := func(g *rig, enforce bool) {
		g.runs.runs["zzzzzzzz"] = runs.Run{ID: "zzzzzzzz", Principal: runs.PrincipalFactory, Phase: "Succeeded",
			Tokens: 25_000_000, Created: now.Add(-time.Hour)}
		g.r.Cfg.Budgets = config.Budgets{EnforcePrincipal: enforce, FactoryDaily: 25_000_000, HumanDaily: 5_000_000}
	}
	g := newRig(t, issueTask("3buqdlot", 7, "x"))
	spent(g, false)
	if tk := g.reconcile(t, "3buqdlot", 3); tk.Status.Phase != v1alpha1.PhaseImplementing {
		t.Fatalf("in shadow the day is counted, not enforced (R3): %s", tk.Status.Phase)
	}
	g = newRig(t, issueTask("3buqdlot", 7, "x"))
	spent(g, true)
	if tk := g.reconcile(t, "3buqdlot", 3); tk.Status.Phase != v1alpha1.PhaseQueued || tk.Status.Reason != "waiting_daily_budget" {
		t.Fatalf("%s %s", tk.Status.Phase, tk.Status.Reason)
	}
	g.r.Now = func() time.Time { return now.Add(24 * time.Hour) }
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Phase != v1alpha1.PhaseImplementing {
		t.Fatalf("a new UTC day starts it: %s", tk.Status.Phase)
	}
}
