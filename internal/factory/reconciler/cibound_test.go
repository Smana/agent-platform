// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"testing"
	"time"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
)

// #182: a wait verdict past merge.awaitingCIFor hands the task to a human, naming what never
// reported; inside the bound, or with no bound (0, the default), it keeps waiting.
func TestAwaitingCIIsBounded(t *testing.T) {
	for name, c := range map[string]struct {
		bound, since  time.Duration
		phase, reason string
	}{
		"no bound, a week waiting": {0, 7 * 24 * time.Hour, v1alpha1.PhaseAwaitingCI, "holdout_pending"},
		"inside the bound":         {24 * time.Hour, 23 * time.Hour, v1alpha1.PhaseAwaitingCI, "holdout_pending"},
		"past the bound":           {24 * time.Hour, 25 * time.Hour, v1alpha1.PhaseAwaitingHuman, "ci_wait_expired"},
	} {
		t.Run(name, func(t *testing.T) {
			g := declaredRig(t)
			g.r.Cfg.Merge.AwaitingCIFor.Duration = c.bound
			g.f.SetChecks(12, green("SUCCESS")) // the holdout never reports
			g.setPhaseSince(t, "3buqdlot", now.Add(-c.since))
			tk := g.reconcile(t, "3buqdlot", 1)
			if tk.Status.Phase != c.phase || tk.Status.Reason != c.reason {
				t.Fatalf("%s %s", tk.Status.Phase, tk.Status.Reason)
			}
			if c.reason == "ci_wait_expired" && !contains(g.f.Comments(12), "External links 🔗") {
				t.Fatalf("the hand-off names the check that never reported: %q", g.f.Comments(12))
			}
		})
	}
}

// ci_pending is a wait like any other: a required check that never reports is bounded too.
func TestAPendingRequiredCheckIsBounded(t *testing.T) {
	g := mergeRig(t, v1alpha1.PhaseAwaitingCI)
	g.r.Cfg.Merge.AwaitingCIFor.Duration = time.Hour
	checks := green("SUCCESS")
	checks.Runs = checks.Runs[1:] // one required check missing
	g.f.SetChecks(12, checks)
	g.setPhaseSince(t, "3buqdlot", now.Add(-2*time.Hour))
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Reason != "ci_wait_expired" {
		t.Fatalf("%s %s", tk.Status.Phase, tk.Status.Reason)
	}
}
