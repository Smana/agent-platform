// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"errors"
	"slices"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/forge"
)

func cancelledChecks(second string) forge.Checks {
	c := green("SUCCESS")
	c.Runs[0].State, c.Runs[0].ID = "CANCELLED", 776
	c.Runs[1].State, c.Runs[1].ID = second, 777
	return c
}

// A job is re-run only once nothing is pending: GitHub refuses it before the run completes.
func TestCancelledBesidePendingWaitsWithoutRerun(t *testing.T) {
	g := mergeRig(t, v1alpha1.PhaseAwaitingCI)
	g.r.Cfg.Merge.CheckReruns = 1
	g.f.SetChecks(12, cancelledChecks("PENDING"))
	tk := g.reconcile(t, "3buqdlot", 3)
	if tk.Status.Phase != v1alpha1.PhaseAwaitingCI || tk.Status.Reason != "ci_pending" || tk.Status.CheckReruns != 0 || len(g.f.Reruns()) != 0 {
		t.Fatalf("%s %q reruns=%d %v", tk.Status.Phase, tk.Status.Reason, tk.Status.CheckReruns, g.f.Reruns())
	}
}

// A refused re-run leaves the cap unspent, and every cancelled job is still tried.
func TestRefusedRerunLeavesTheCapUnspent(t *testing.T) {
	g := mergeRig(t, v1alpha1.PhaseAwaitingCI)
	g.r.Cfg.Merge.CheckReruns = 1
	g.f.SetChecks(12, cancelledChecks("CANCELLED"))
	g.f.FailReruns(errors.New("403"), 776, 777)
	var err error
	for range 1 {
		_, err = g.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "agent-system", Name: "3buqdlot"}})
	}
	if err == nil || len(g.f.Reruns()) < 2 {
		t.Fatalf("the refusal is returned after both jobs were tried: %v %v", err, g.f.Reruns())
	}
	var tk v1alpha1.Task
	_ = g.c.Get(t.Context(), client.ObjectKey{Namespace: "agent-system", Name: "3buqdlot"}, &tk)
	if tk.Status.CheckReruns != 0 || tk.Status.Phase != v1alpha1.PhaseAwaitingCI {
		t.Fatalf("%s reruns=%d", tk.Status.Phase, tk.Status.CheckReruns)
	}
}

// A new head gets a fresh re-run budget.
func TestNewHeadResetsTheRerunBudget(t *testing.T) {
	g := mergeRig(t, v1alpha1.PhaseAwaitingCI)
	g.r.Cfg.Merge.CheckReruns = 1
	g.f.SetChecks(12, cancelledChecks("SUCCESS"))
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.CheckReruns != 1 {
		t.Fatalf("first head: %d", tk.Status.CheckReruns)
	}
	g.f.SetPR(forge.PR{Number: 12, NodeID: "PR_12", State: "OPEN", Title: "docs: fix a link", Author: "ogenki-agents[bot]",
		HeadRef: "agent/3buqdlot", HeadSHA: "def", HeadMessage: "docs: fix"})
	tk := g.reconcile(t, "3buqdlot", 1)
	if tk.Status.Phase != v1alpha1.PhaseAwaitingCI || tk.Status.CheckReruns != 1 || tk.Status.CheckRerunsHead != "def" ||
		!slices.Equal(g.f.Reruns(), []int64{776, 776}) {
		t.Fatalf("%s reruns=%d head=%q %v", tk.Status.Phase, tk.Status.CheckReruns, tk.Status.CheckRerunsHead, g.f.Reruns())
	}
}

// N1: a ci_cancelled hand-off is transient; a green CI on the same head returns the task to the gate.
func TestACancelledHandOffReturnsToTheGate(t *testing.T) {
	g := mergeRig(t, v1alpha1.PhaseAwaitingCI)
	g.r.Cfg.Merge.CheckReruns = 0
	g.f.SetChecks(12, cancelledChecks("SUCCESS"))
	if tk := g.reconcile(t, "3buqdlot", 2); tk.Status.Phase != v1alpha1.PhaseAwaitingHuman || tk.Status.Reason != "ci_cancelled" {
		t.Fatalf("%s %s", tk.Status.Phase, tk.Status.Reason)
	}
	g.f.SetChecks(12, cancelledChecks("CANCELLED")) // still cancelled: it stays with the human
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Phase != v1alpha1.PhaseAwaitingHuman {
		t.Fatalf("still cancelled: %s", tk.Status.Phase)
	}
	g.f.SetChecks(12, green("SUCCESS"))
	tk := g.reconcile(t, "3buqdlot", 1)
	if tk.Status.Phase != v1alpha1.PhaseAwaitingCI || tk.Status.Reason != "reentered_ci_green" || tk.Status.GateReentries != 1 {
		t.Fatalf("%s %s %d", tk.Status.Phase, tk.Status.Reason, tk.Status.GateReentries)
	}
}

// A cancelled watched check on main is neither green nor red: the task keeps verifying.
func TestVerifyingNeverReadsCancelledAsGreen(t *testing.T) {
	g := mergeRig(t, v1alpha1.PhaseVerifying)
	var tk v1alpha1.Task
	_ = g.c.Get(t.Context(), client.ObjectKey{Namespace: "agent-system", Name: "3buqdlot"}, &tk)
	merged := metav1.NewTime(now)
	tk.Status.PullRequest.MergeCommitSHA, tk.Status.PullRequest.AutoMerged, tk.Status.PullRequest.MergedAt = "m1", true, &merged
	_ = g.c.Status().Update(t.Context(), &tk)
	g.f.SetCommitChecks("m1", forge.Check{Name: required[0], State: "SUCCESS"}, forge.Check{Name: required[1], State: "CANCELLED"})
	g.r.Now = func() time.Time { return now.Add(31 * time.Minute) }
	got := g.reconcile(t, "3buqdlot", 1)
	if got.Status.Phase != v1alpha1.PhaseVerifying {
		t.Fatalf("%s %s", got.Status.Phase, got.Status.Reason)
	}
}
