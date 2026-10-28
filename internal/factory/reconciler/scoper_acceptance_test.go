// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"slices"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/Smana/maku/api/factory/v1alpha1"
)

// The issues of the 2026-10-27 gcp-0 drills: neither states an acceptance check.
const (
	drillA = "docs: bring the Status section up to date\n\nThe Status section of docs/factory.md looks stale. Please bring it up to date."
	drillB = "docs: define 'room' in the runbooks README\n\nThe runbooks README uses the word 'room' without saying what it is. Add a definition."
	withAC = "docs: define 'room' in the runbooks README\n\n## What is wrong\nThe README uses 'room' undefined.\n\n" +
		"## Acceptance check\nThe README has a sentence defining a room.\n\nFix it; change nothing else."
)

func scoperOn(t *testing.T, text string) *rig {
	t.Helper()
	return scoperRigOf(t, func(tk *v1alpha1.Task) { tk.Spec.Text = text })
}

// The owner's rule: the scoper asks when the issue states no acceptance check, whatever it says.
func TestAnIssueWithoutAcceptanceBouncesEvenWhenTheScoperSaysReady(t *testing.T) {
	for name, text := range map[string]string{"drill B": drillB, "drill A": drillA,
		"an empty section": "t\n\n## Acceptance check\n\n## Notes\nx"} {
		t.Run(name, func(t *testing.T) {
			g := scoperOn(t, text)
			before := len(g.f.Comments(7))
			scopeHandoff(g, 0, readyScope())
			tk := endScoper(t, g, "Succeeded", "agent_finished")
			if tk.Status.Phase != v1alpha1.PhaseEscalated || tk.Status.Reason != "needs_author" || tk.Status.Scope.State != "question" {
				t.Fatalf("%s %s %+v", tk.Status.Phase, tk.Status.Reason, tk.Status.Scope)
			}
			c := g.f.Comments(7)[before:]
			if len(c) != 1 || !strings.Contains(c[0], "acceptance check") || !strings.Contains(c[0], "Assumption:") {
				t.Fatalf("comments %q", c)
			}
			if !slices.Equal(g.f.Added(7), []string{"factory/needs-author"}) || len(g.runs.specs) != 1 {
				t.Fatalf("labels %v, %d runs: no implementer may start", g.f.Added(7), len(g.runs.specs))
			}
		})
	}
}

func TestAnIssueWithAcceptanceScopesReady(t *testing.T) {
	g := scoperOn(t, withAC)
	scopeHandoff(g, 0, readyScope())
	tk := endScoper(t, g, "Succeeded", "agent_finished")
	if tk.Status.Phase != v1alpha1.PhaseQueued || tk.Status.Scope.State != "ready" {
		t.Fatalf("%s %+v", tk.Status.Phase, tk.Status.Scope)
	}
}

// A schedule has no issue author to ask, and its text is reviewed configuration.
func TestAScheduleNeedsNoAcceptance(t *testing.T) {
	g := scoperRigOf(t, func(tk *v1alpha1.Task) { tk.Spec.Issue, tk.Spec.Source.Kind, tk.Spec.Text = 0, "schedule", drillB })
	scopeHandoff(g, 0, readyScope())
	if got := endScoper(t, g, "Succeeded", "agent_finished"); got.Status.Phase != v1alpha1.PhaseQueued || got.Status.Scope.State != "ready" {
		t.Fatalf("%s %+v", got.Status.Phase, got.Status.Scope)
	}
}

// SC8 (b): after a bounce for a missing acceptance, /factory retry proceeds on the assumption.
func TestARetryAfterTheAcceptanceBounceProceedsAssumed(t *testing.T) {
	g := scoperOn(t, drillB)
	scopeHandoff(g, 0, readyScope())
	endScoper(t, g, "Succeeded", "agent_finished")
	g.r.Now = func() time.Time { return now.Add(time.Hour) }
	g.f.SetComments(7, retryBy(77, "smana", -30*time.Minute))
	tk := g.reconcile(t, "3buqdlot", 2)
	s := g.runs.specs[rid(1)]
	if tk.Status.Scope.State != "assumed" || s.Role != "implementer" || !strings.Contains(s.TaskText, "first assumption") {
		t.Fatalf("%+v %+v", tk.Status.Scope, s)
	}
}
