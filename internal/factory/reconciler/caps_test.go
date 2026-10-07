// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"slices"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/forge"
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

// The task cap is checked at admission, before every run of the task: a run starts only while one
// whole RunTokens still fits under TaskTokens (owner, 2026-10-07), the rule a resume follows too.
// Standard tier: 1.5 M per run, 3 M per task. Two paths into a run: a maintainer's revision and the
// pair's reviewer, the run a 3.35 M task kept starting while the cap was shadow (aws-0, 2026-10-06).
func TestTaskTokenCapLeavesRoomForAWholeRun(t *testing.T) {
	paths := map[string]func(t *testing.T, used int64, enforce bool) (*rig, *v1alpha1.Task){
		"revision": func(t *testing.T, used int64, enforce bool) (*rig, *v1alpha1.Task) {
			tk := awaiting()
			tk.Status.Phase, tk.Status.NextTrigger = v1alpha1.PhaseQueued, "human"
			tk.Spec.Budget.TaskTokens = 3_000_000
			// observe rebuilds the total from the records (R49); a record keeps its last reading
			// when its claim is gone.
			tk.Status.Runs[0].Tokens = used
			g := newRig(t, tk, roomOf("3buqdlot"))
			g.f.SetPR(pr12()) // Queued re-reads the task's pull request (lateReviews) first
			g.r.Cfg.Budgets.EnforceTask = enforce
			return g, g.reconcile(t, "3buqdlot", 1)
		},
		"reviewer": func(t *testing.T, used int64, enforce bool) (*rig, *v1alpha1.Task) {
			g := newRig(t, issueTask("3buqdlot", 7, "x"))
			g.r.Triage = staticWith("pair")
			g.r.Cfg.Budgets.EnforceTask = enforce
			g.reconcile(t, "3buqdlot", 3)
			g.f.SetBranch("agent/3buqdlot", 12)
			g.f.SetPR(pr12At(head1))
			r := g.runs.runs[rid(0)]
			r.Tokens = used
			g.runs.runs[rid(0)] = r
			g.finish(rid(0), "Succeeded", "agent_finished")
			return g, g.reconcile(t, "3buqdlot", 2) // → Queued for the reviewer → admitted or not
		},
	}
	for path, start := range paths {
		for name, c := range map[string]struct {
			used    int64
			enforce bool
			started bool
		}{
			"a run's worth left":                  {1_500_000, true, true},
			"less than a run's worth left":        {1_500_001, true, false},
			"in shadow, counted but not enforced": {1_500_001, false, true},
		} {
			t.Run(path+"/"+name, func(t *testing.T) {
				g, tk := start(t, c.used, c.enforce)
				_, started := g.runs.specs[rid(1)]
				if started != c.started {
					t.Fatalf("run started %v, want %v: %s %s", started, c.started, tk.Status.Phase, tk.Status.Reason)
				}
				shadow := slices.Contains(g.metrics.recorded, "revoked budget-task-shadow")
				if shadow != (c.started && c.used > 1_500_000) {
					t.Fatalf("shadow counted %v: %q", shadow, g.metrics.recorded)
				}
				if c.started {
					return
				}
				if tk.Status.Phase != v1alpha1.PhaseEscalated || tk.Status.Reason != "budget-task" {
					t.Fatalf("%s %s", tk.Status.Phase, tk.Status.Reason)
				}
				if say := strings.Join(g.f.Comments(7), "\n"); !strings.Contains(say, "no room left for another full run") ||
					!strings.Contains(say, "Tokens used: 1.5 M") {
					t.Fatalf("the escalation says why, honestly: %q", say)
				}
			})
		}
	}
}

// A task queued for its reviewer whose pull request was merged meanwhile needs no run: the late
// review path ends it, and the task cap, which only guards a run's start, never escalates it.
func TestAMergedPullRequestIsNotEscalatedForTheTaskCap(t *testing.T) {
	g := newRig(t, issueTask("3buqdlot", 7, "x"))
	g.r.Triage = staticWith("pair")
	g.r.Cfg.Budgets.EnforceTask = true
	g.reconcile(t, "3buqdlot", 3)
	g.f.SetBranch("agent/3buqdlot", 12)
	g.f.SetPR(pr12At(head1))
	r := g.runs.runs[rid(0)]
	r.Tokens = 1_500_001 // less than a run's worth left
	g.runs.runs[rid(0)] = r
	g.finish(rid(0), "Succeeded", "agent_finished")
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Phase != v1alpha1.PhaseQueued || tk.Status.NextRole != "reviewer" {
		t.Fatalf("queued for the reviewer: %s %q", tk.Status.Phase, tk.Status.NextRole)
	}
	g.f.SetPR(forge.PR{Number: 12, State: "MERGED", MergedBy: "Smana", HeadSHA: head1})
	// Not even for one reconcile: Escalated would itself find the merge, but only after pinging
	// the maintainers about a budget the task no longer needs.
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Phase != v1alpha1.PhaseAwaitingHuman {
		t.Fatalf("%s %s", tk.Status.Phase, tk.Status.Reason)
	}
	tk := g.reconcile(t, "3buqdlot", 2)
	if tk.Status.Phase != v1alpha1.PhaseDone || len(g.runs.specs) != 1 {
		t.Fatalf("%s %s, %d runs", tk.Status.Phase, tk.Status.Reason, len(g.runs.specs))
	}
	if say := strings.Join(g.f.Comments(7), "\n"); strings.Contains(say, "no room left") || strings.Contains(say, "needs a maintainer") {
		t.Fatalf("no escalation was narrated: %q", say)
	}
}

// A maintainer's "Request changes" on a task with no room left for a run escalates it as
// budget-task without first announcing a revision that cannot start; the review is still queued
// in the room for whoever picks the task up. In shadow the revision is announced and starts.
// Two ways in: the review lands while the task awaits a human, or while it is queued for its
// reviewer.
func TestARequestChangesPastTheCapIsQueuedNotAnnounced(t *testing.T) {
	review := forge.Review{ID: 901, Author: "Smana", State: "CHANGES_REQUESTED", Body: "Use the relative link.", At: now.Add(time.Minute)}
	paths := map[string]func(t *testing.T, enforce bool) (*rig, *v1alpha1.Task){
		"awaiting a human": func(t *testing.T, enforce bool) (*rig, *v1alpha1.Task) {
			tk := awaiting()
			tk.Spec.Budget.TaskTokens = 3_000_000
			tk.Status.Runs[0].Tokens, tk.Status.Usage.Tokens = 1_500_001, 1_500_001
			g := newRig(t, tk, roomOf("3buqdlot"))
			g.f.SetPR(pr12(review))
			g.r.Cfg.Budgets.EnforceTask = enforce
			return g, g.reconcile(t, "3buqdlot", 2) // → Queued → admitted or not
		},
		"queued for its reviewer": func(t *testing.T, enforce bool) (*rig, *v1alpha1.Task) {
			g := newRig(t, issueTask("3buqdlot", 7, "x"))
			g.r.Triage = staticWith("pair")
			g.r.Cfg.Budgets.EnforceTask = enforce
			g.reconcile(t, "3buqdlot", 3)
			g.f.SetBranch("agent/3buqdlot", 12)
			g.f.SetPR(pr12At(head1))
			r := g.runs.runs[rid(0)]
			r.Tokens = 1_500_001
			g.runs.runs[rid(0)] = r
			g.finish(rid(0), "Succeeded", "agent_finished")
			g.reconcile(t, "3buqdlot", 1) // → Queued for the reviewer
			pr := pr12At(head1)
			pr.Reviews = []forge.Review{review}
			g.f.SetPR(pr)
			return g, g.reconcile(t, "3buqdlot", 1)
		},
	}
	for path, start := range paths {
		for name, enforce := range map[string]bool{"enforced": true, "shadow": false} {
			t.Run(path+"/"+name, func(t *testing.T) {
				g, tk := start(t, enforce)
				if !slices.Equal(g.log.reviews(), []int64{901}) {
					t.Fatalf("the review is queued in the room either way: %v", g.log.reviews())
				}
				say := strings.Join(g.f.Comments(7), "\n")
				announced := strings.Contains(say, "is revising after @Smana's review")
				_, started := g.runs.specs[rid(1)]
				if enforce {
					if tk.Status.Phase != v1alpha1.PhaseEscalated || tk.Status.Reason != "budget-task" || announced || started {
						t.Fatalf("%s %s, started %v: %q", tk.Status.Phase, tk.Status.Reason, started, say)
					}
					return
				}
				if !announced || !started || g.runs.specs[rid(1)].Role != "implementer" {
					t.Fatalf("in shadow the revision is announced and starts: %s, started %v: %q", tk.Status.Phase, started, say)
				}
			})
		}
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
