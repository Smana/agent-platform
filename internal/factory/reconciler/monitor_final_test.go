// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"slices"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/Smana/maku/api/factory/v1alpha1"
	"github.com/Smana/maku/internal/factory/config"
	"github.com/Smana/maku/internal/factory/forge"
)

// Declaring a monitor never back-fills history: a Done task whose followUpWindow (counted from
// GitHub's mergedAt) is already over starts no watch, so it reads, labels and says nothing.
func TestDeclaringAMonitorDoesNotBackfillOldMerges(t *testing.T) {
	g := monitorRig(t)
	g.setDone(t, "3buqdlot", 12, "Smana", mSHA, now.Add(-30*24*time.Hour))
	g.f.SetCommitChecks(mSHA, forge.Check{Name: "Pre-commit checks", State: "FAILURE"}) // a long-ago red
	cf := &countingForge{TaskForge: g.f}
	g.r.Forge = cf
	tk := g.reconcile(t, "3buqdlot", 2)
	if tk.Status.Monitor != nil || g.r.monitorDue(tk) {
		t.Fatalf("no watch for a merge past its window: %+v", tk.Status.Monitor)
	}
	if cf.prs != 0 || len(g.f.Added(12)) != 0 || len(g.f.Comments(12)) != 0 || len(g.f.Comments(g.r.Cfg.ControlFor().Issue)) != 0 {
		t.Fatalf("no reads, labels or comments: %d reads, %v, %q", cf.prs, g.f.Added(12), g.f.Comments(12))
	}
	// a merge still inside its window is watched as before
	g = monitorRig(t)
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Monitor == nil {
		t.Fatal("a recent merge is watched")
	}
}

// The control issue lives in control.repository: another repository's task posts nothing on an
// issue that merely shares its number, for the monitor's copies and for a revert's.
func TestControlCopiesStayOnTheControlRepository(t *testing.T) {
	g := monitorRig(t)
	g.r.Cfg.Control = config.Control{Repository: "Smana/elsewhere", Issue: 3}
	g.f.SetCommitChecks(mSHA, forge.Check{Name: "Pre-commit checks", State: "FAILURE"})
	g.f.SetMerged(later(20, "Smana", "x", FollowUpOfLabel(12)))
	tk := g.reconcile(t, "3buqdlot", 1)
	if tk.Status.Monitor.Verdict != "regression" || !slices.Contains(tk.Status.Monitor.FollowUps, 20) {
		t.Fatalf("%+v", tk.Status.Monitor)
	}
	if !contains(g.f.Comments(12), "monitor-12 verdict=regression") || len(g.f.Comments(3)) != 0 {
		t.Fatalf("the PR hears of it, issue 3 of another repository does not: %q %q", g.f.Comments(12), g.f.Comments(3))
	}
	// the revert's copy, the same bug at the same pattern
	for name, repo := range map[string]string{"other": "Smana/elsewhere", "own": "Smana/cloud-native-ref"} {
		t.Run(name, func(t *testing.T) {
			g := mergeRig(t, v1alpha1.PhaseVerifying)
			g.r.Cfg.Control = config.Control{Repository: repo, Issue: 1}
			var tk v1alpha1.Task
			_ = g.c.Get(t.Context(), client.ObjectKey{Namespace: "agent-system", Name: "3buqdlot"}, &tk)
			merged := metav1.NewTime(now)
			tk.Status.PullRequest.MergeCommitSHA, tk.Status.PullRequest.AutoMerged, tk.Status.PullRequest.MergedAt = "m1", true, &merged
			_ = g.c.Status().Update(t.Context(), &tk)
			red := green("SUCCESS")
			red.Runs[0].State = "FAILURE"
			g.f.SetCommitChecks("m1", red.Runs...)
			g.reconcile(t, "3buqdlot", 1)
			posted := len(g.f.Comments(1)) == 1 && strings.Contains(g.f.Comments(1)[0], "human review")
			if posted != (name == "own") {
				t.Fatalf("%s: %q", name, g.f.Comments(1))
			}
		})
	}
}

// Off by default: a shadow or live config with none of the new keys reads exactly what v0.20.0
// read, the auto-merged list per class and the revert of each merge in its window.
func TestTheLedgerReadsOnlyWhatV020ReadWithoutTheNewKeys(t *testing.T) {
	g := declaredRig(t) // classes live, no monitor, no maxFollowUps
	for i := range 3 {
		g.f.SetPR(forge.PR{Number: 40 + i, State: "MERGED", MergedAt: now.Add(-time.Duration(i+1) * time.Hour),
			Labels: []string{AutoMergedLabel("dead-links")}})
	}
	g.reconcile(t, "3buqdlot", 1)
	reads := g.f.LabelReads()
	for _, l := range reads {
		if !strings.HasPrefix(l, "factory/auto-merged:") && !strings.HasPrefix(l, "factory/revert-of:#") {
			t.Fatalf("a new read without a new key: %q in %v", l, reads)
		}
	}
	if len(reads) == 0 {
		t.Fatal("the ledger was read")
	}
	// with a monitor declared, the extended reads start
	g = monitorRig(t)
	g.reconcile(t, "3buqdlot", 1)
	var tk v1alpha1.Task
	_ = g.c.Get(t.Context(), keyFor("3buqdlot"), &tk)
	if _, err := g.r.githubLedger(t.Context(), g.r.repo(&tk)); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(g.f.LabelReads(), RegressionLabel) || !slices.Contains(g.f.LabelReads(), MergedLabel("dead-links")) {
		t.Fatalf("%v", g.f.LabelReads())
	}
}
