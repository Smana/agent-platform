// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/Smana/maku/api/factory/v1alpha1"
	"github.com/Smana/maku/internal/factory/config"
	"github.com/Smana/maku/internal/factory/forge"
)

// handedOff puts the adopted rig's Task in AwaitingHuman with the given reason, on the head it was
// decided at (adoptHead), as a hand-off leaves it.
func handedOff(t *testing.T, g *rig, reason string) {
	t.Helper()
	tk := g.reconcile(t, adoptTask, 0)
	tk.Status.Phase, tk.Status.Reason = v1alpha1.PhaseAwaitingHuman, reason
	if err := g.c.Status().Update(t.Context(), tk); err != nil {
		t.Fatal(err)
	}
}

func liveClass(g *rig) {
	cl := g.r.Cfg.Classes["deps-patch"]
	cl.Live, cl.Shadow = true, false
	g.r.Cfg.Classes["deps-patch"] = cl
}

// R45, amended: every transient adopted hand-off returns on a new head, counted; in a live class
// so do the cap and the pause. Another reason, or the same head, stays.
func TestAnAdoptedTransientHandOffReturnsOnANewHead(t *testing.T) {
	for _, c := range []struct {
		reason string
		live   bool
		back   bool
	}{
		{"policy_pending", false, true}, {"policy_absent", false, true}, {"ci_cancelled", false, true},
		{"ci_wait_expired", false, true}, {"auto_merge_cap", true, true}, {"class_paused", true, true},
		{"auto_merge_cap", false, true}, {"class_paused", false, true}, {"changes_requested", false, false},
		{"file_not_allowed", false, false},
	} {
		t.Run(c.reason, func(t *testing.T) {
			g := adoptRig(t)
			if c.live {
				liveClass(g)
			}
			handedOff(t, g, c.reason)
			if tk := g.reconcile(t, adoptTask, 1); tk.Status.Phase != v1alpha1.PhaseAwaitingHuman {
				t.Fatalf("the same head stays: %s %s", tk.Status.Phase, tk.Status.Reason)
			}
			g.cleanHead(newHead)
			tk := g.reconcile(t, adoptTask, 1)
			if got := tk.Status.Phase == v1alpha1.PhaseAwaitingCI; got != c.back || (c.back && tk.Status.GateReentries != 1) {
				t.Fatalf("returns %v, want %v: %s %s %d", got, c.back, tk.Status.Phase, tk.Status.Reason, tk.Status.GateReentries)
			}
		})
	}
	// the cap of three returns stands.
	g := adoptRig(t)
	handedOff(t, g, "policy_pending")
	tk := g.reconcile(t, adoptTask, 0)
	tk.Status.GateReentries = maxGateReentries
	_ = g.c.Status().Update(t.Context(), tk)
	g.cleanHead(newHead)
	if tk = g.reconcile(t, adoptTask, 1); tk.Status.Phase != v1alpha1.PhaseAwaitingHuman {
		t.Fatalf("past the cap: %s", tk.Status.Phase)
	}
}

// N3: the in-process breaker window matches the label ledger: an adopted merge of a head the gate
// never decided (Renovate's automerge) is not counted, whatever its monitor says.
func TestPausedIgnoresAnUndecidedAdoptedMerge(t *testing.T) {
	for name, decided := range map[string]string{"decided": "9999999999999999999999999999999999999999", "undecided": adoptHead} {
		t.Run(name, func(t *testing.T) {
			g := adoptRig(t)
			g.r.Cfg.Merge.Breaker = config.Breaker{Window: 10, MaxReverts: 1, MaxFollowUps: 1}
			var tk v1alpha1.Task
			if err := g.c.Get(t.Context(), client.ObjectKey{Namespace: "agent-system", Name: adoptTask}, &tk); err != nil {
				t.Fatal(err)
			}
			at := metav1.NewTime(now.Add(-time.Hour))
			tk.Status.Phase, tk.Status.Reason = v1alpha1.PhaseDone, "merged"
			tk.Status.PullRequest.MergedAt, tk.Status.PullRequest.MergedBy = &at, bot
			tk.Status.PullRequest.HeadSHA, tk.Status.PullRequest.DecidedHeadSHA = "9999999999999999999999999999999999999999", decided
			tk.Status.Monitor = &v1alpha1.MonitorStatus{Verdict: "regression"}
			if err := g.c.Status().Update(t.Context(), &tk); err != nil {
				t.Fatal(err)
			}
			paused, err := g.r.paused(t.Context(), g.r.repo(&tk), "deps-patch")
			if err != nil {
				t.Fatal(err)
			}
			if want := name == "decided"; paused != want {
				t.Fatalf("paused %v, want %v", paused, want)
			}
		})
	}
}

// R35 amended: an adopted merge is watched only at a head the gate decided (shadow_would_arm or
// arm). Renovate's automerge of a PR the gate refused, or never decided, is not labelled
// factory/merged:<class>, so it feeds neither the breaker nor the tally.
func TestAnAdoptedMergeIsWatchedOnlyAtADecidedHead(t *testing.T) {
	merge := func(g *rig) *v1alpha1.Task {
		g.editPR(func(p *forge.PR) {
			p.State, p.MergedBy, p.MergedAt, p.MergeCommitSHA = "MERGED", bot, now, "9999999999999999999999999999999999999999"
		})
		return g.reconcile(t, adoptTask, 1)
	}
	monitored := func(g *rig) *rig {
		cl := g.r.Cfg.Classes["deps-patch"]
		cl.Monitor = &config.Monitor{FollowUpWindow: config.Duration{Duration: 168 * time.Hour}}
		g.r.Cfg.Classes["deps-patch"] = cl
		return g
	}
	t.Run("the decided head", func(t *testing.T) {
		g := monitored(adoptRig(t))
		g.reconcile(t, adoptTask, 1) // shadow_would_arm at adoptHead
		tk := merge(g)
		if !g.r.monitorDue(tk) {
			t.Fatalf("a merge of the decided head is watched: %+v", tk.Status.PullRequest)
		}
		g.reconcile(t, adoptTask, 1)
		if !slices.Contains(g.f.Added(30), MergedLabel("deps-patch")) {
			t.Fatalf("labels %v", g.f.Added(30))
		}
	})
	t.Run("a head merged before it was decided", func(t *testing.T) {
		g := monitored(adoptRig(t))
		g.reconcile(t, adoptTask, 1)
		g.cleanHead(newHead) // Renovate rebased and its automerge landed before the gate read it
		tk := merge(g)
		if tk.Status.Phase != v1alpha1.PhaseDone || g.r.monitorDue(tk) {
			t.Fatalf("%s due %v", tk.Status.Phase, g.r.monitorDue(tk))
		}
		g.reconcile(t, adoptTask, 2)
		if slices.Contains(g.f.Added(30), MergedLabel("deps-patch")) {
			t.Fatalf("an undecided merge is not the class's: %v", g.f.Added(30))
		}
	})
	t.Run("a pull request the gate refused", func(t *testing.T) {
		g := monitored(adoptRig(t))
		g.review(30, 7, "Smana", "CHANGES_REQUESTED")
		g.reconcile(t, adoptTask, 1) // changes_requested: handed off, never decided
		if tk := merge(g); tk.Status.Phase != v1alpha1.PhaseDone || g.r.monitorDue(tk) {
			t.Fatalf("%s due %v", tk.Status.Phase, g.r.monitorDue(tk))
		}
	})
}

// A review counts toward the adopted veto only from an owner, member or collaborator; a ghost or an
// association GitHub did not say still vetoes, and so does a cut list.
func TestTheAdoptedVetoIsARepositoryMembers(t *testing.T) {
	for name, c := range map[string]struct {
		rv     forge.Review
		vetoed bool
	}{
		"a stranger":               {forge.Review{ID: 7, Author: "rando", State: "CHANGES_REQUESTED", Association: "NONE"}, false},
		"a first-time contributor": {forge.Review{ID: 7, Author: "rando", State: "CHANGES_REQUESTED", Association: "FIRST_TIME_CONTRIBUTOR"}, false},
		"a collaborator":           {forge.Review{ID: 7, Author: "Smana", State: "CHANGES_REQUESTED", Association: "COLLABORATOR"}, true},
		"a member":                 {forge.Review{ID: 7, Author: "Smana", State: "CHANGES_REQUESTED", Association: "MEMBER"}, true},
		"an owner":                 {forge.Review{ID: 7, Author: "Smana", State: "CHANGES_REQUESTED", Association: "OWNER"}, true},
		"a ghost":                  {forge.Review{ID: 7, Author: "", State: "CHANGES_REQUESTED"}, true},
		"an association not said":  {forge.Review{ID: 7, Author: "Smana", State: "CHANGES_REQUESTED"}, true},
		"a dismissed stranger":     {forge.Review{ID: 7, Author: "rando", State: "DISMISSED", Association: "NONE"}, false},
		"a dismissed member":       {forge.Review{ID: 7, Author: "Smana", State: "DISMISSED", Association: "MEMBER"}, true},
		"a dismissed ghost":        {forge.Review{ID: 7, Author: "", State: "DISMISSED"}, true},
		"a stranger's comment":     {forge.Review{ID: 7, Author: "rando", State: "COMMENTED", Association: "NONE"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			g := adoptRig(t)
			g.editPR(func(p *forge.PR) { p.Reviews = append(p.Reviews, c.rv) })
			tk := g.reconcile(t, adoptTask, 1)
			got := tk.Status.Reason == "changes_requested"
			if got != c.vetoed || (!c.vetoed && tk.Status.Reason != "shadow_would_arm") {
				t.Fatalf("vetoed %v, want %v: %s %s", got, c.vetoed, tk.Status.Phase, tk.Status.Reason)
			}
		})
	}
	g := adoptRig(t)
	g.editPR(func(p *forge.PR) { p.ReviewsTruncated = true })
	if tk := g.reconcile(t, adoptTask, 1); tk.Status.Reason != "changes_requested" {
		t.Fatalf("a cut list vetoes: %s", tk.Status.Reason)
	}
}

// M1: an adopted Task whose class was removed or no longer adopts never reads room "": it hands off.
func TestAnAdoptedTaskWhoseClassIsGoneHandsOff(t *testing.T) {
	for name, edit := range map[string]func(*rig){
		"removed": func(g *rig) { g.r.Cfg.Classes = map[string]config.Class{"other": {Shadow: true}} },
		"no author": func(g *rig) {
			cl := g.r.Cfg.Classes["deps-patch"]
			cl.Author = nil
			g.r.Cfg.Classes["deps-patch"] = cl
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := adoptRig(t)
			edit(g)
			tk := g.reconcile(t, adoptTask, 1)
			if tk.Status.Phase != v1alpha1.PhaseAwaitingHuman || tk.Status.Reason != "class_mismatch" {
				t.Fatalf("%s %s", tk.Status.Phase, tk.Status.Reason)
			}
		})
	}
}
