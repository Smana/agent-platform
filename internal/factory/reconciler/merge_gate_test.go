// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/forge"
)

// declaredRig is mergeRig's task in a class that declares conditions (MC1): its gate reads the
// diff and the holdout, as dead-links' will.
func declaredRig(t *testing.T) *rig {
	g := mergeRig(t, v1alpha1.PhaseAwaitingCI)
	var tk v1alpha1.Task
	if err := g.c.Get(t.Context(), keyFor("3buqdlot"), &tk); err != nil {
		t.Fatal(err)
	}
	tk.Spec.PredictedClass = "dead-links"
	if err := g.c.Update(t.Context(), &tk); err != nil {
		t.Fatal(err)
	}
	g.r.Cfg.Classes["dead-links"] = declaredClass()
	checks := green("SUCCESS")
	checks.Runs = append(checks.Runs, forge.Check{Name: "External links 🔗", State: "SUCCESS", App: forge.ActionsApp, Conclusion: "SUCCESS"})
	g.f.SetChecks(12, checks)
	g.f.SetFiles(map[string]string{testFile: "[x](https://example.com/old)"}, map[string]string{testFile: "[x](https://example.com/new)"})
	return g
}

// The second pass reads the diff for any class with a condition on it, and decides on GitHub's
// own list of the changed files.
func TestADeclaredClassIsDecidedOnItsDiff(t *testing.T) {
	g := declaredRig(t)
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Phase != v1alpha1.PhaseAutoMerging || len(g.f.Armed()) != 1 {
		t.Fatalf("a clean retarget arms: %s %s", tk.Status.Phase, tk.Status.Reason)
	}
	g = declaredRig(t)
	g.f.SetFileStats(forge.FileStat{Path: testFile, Status: "modified", Additions: 15, Deletions: 6})
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Phase != v1alpha1.PhaseAwaitingHuman || tk.Status.Reason != "patch_too_large" ||
		len(g.f.Armed()) != 0 {
		t.Fatalf("21 lines refuse: %s %s", tk.Status.Phase, tk.Status.Reason)
	}
}

// maxPerDay counts only the class's own merges: another class's merge today leaves room.
func TestMaxPerDayCountsTheClassOnly(t *testing.T) {
	g := declaredRig(t)
	g.f.SetPR(forge.PR{Number: 9, State: "MERGED", MergedBy: "ogenki-agent-merger[bot]", MergedAt: now.Add(-time.Hour),
		Labels: []string{AutoMergedLabel("docs-links")}})
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Phase != v1alpha1.PhaseAutoMerging {
		t.Fatalf("docs-links' merge is not dead-links': %s %s", tk.Status.Phase, tk.Status.Reason)
	}
	g = declaredRig(t)
	g.f.SetPR(forge.PR{Number: 9, State: "MERGED", MergedBy: "ogenki-agent-merger[bot]", MergedAt: now.Add(-time.Hour),
		Labels: []string{AutoMergedLabel("dead-links")}})
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Reason != "auto_merge_cap" {
		t.Fatalf("one dead-links merge today fills its day: %s %s", tk.Status.Phase, tk.Status.Reason)
	}
}

// A pending holdout waits in AwaitingCI, like CI itself.
func TestAPendingHoldoutWaits(t *testing.T) {
	g := declaredRig(t)
	checks := green("SUCCESS")
	checks.Runs = append(checks.Runs, forge.Check{Name: "External links 🔗", State: "PENDING", App: forge.ActionsApp})
	g.f.SetChecks(12, checks)
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Phase != v1alpha1.PhaseAwaitingCI || tk.Status.Reason != "holdout_pending" {
		t.Fatalf("%s %s", tk.Status.Phase, tk.Status.Reason)
	}
}

// A diff of a full page is unseen files: the task is handed off, not retried in AwaitingCI.
func TestAFullPageDiffRefusesPatchTooLarge(t *testing.T) {
	g := declaredRig(t)
	g.f.SetDiffError(fmt.Errorf("wrapped: %w", forge.ErrDiffTooLarge))
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Phase != v1alpha1.PhaseAwaitingHuman || tk.Status.Reason != "patch_too_large" ||
		len(g.f.Armed()) != 0 {
		t.Fatalf("%s %s", tk.Status.Phase, tk.Status.Reason)
	}
	g = declaredRig(t)
	g.f.SetDiffError(errors.New("boom"))
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "agent-system", Name: "3buqdlot"}}
	if _, err := g.r.Reconcile(t.Context(), req); err == nil || len(g.f.Armed()) != 0 {
		t.Fatalf("another Diff error stays an error, failing closed: %v", err)
	}
}
