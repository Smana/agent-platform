// SPDX-License-Identifier: Apache-2.0

package fmetrics

import (
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// v2 D10: the task, outcome, intake, intervention and time-to-PR series carry a repository label
// whose values come from the config list only; anything else, an empty one included, is other.
func TestTheRepositoryLabelIsBoundedByTheList(t *testing.T) {
	onA, stray := tk("a", "Implementing"), tk("s", "Implementing")
	onA.Spec.Repository, stray.Spec.Repository = "smana/A", "Evil/repo"
	c := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(onA, stray).Build()
	s, scrape := rig(t, c, func() bool { return true })
	s.ListRepositories("Smana/a", "Smana/b")
	b := WithRepository(t.Context(), "SMANA/b")
	s.PROutcome(b, "docs-links", "closed")
	s.TimeToPR(b, time.Minute, "issue", "standard", "solo")
	s.TaskTokens(b, 1000, "standard", "solo", "review")
	s.Intervention(b, "stop")
	s.IntakeError(b, "issue")
	s.PROutcome(WithRepository(t.Context(), "Evil/repo"), "docs-links", "closed")
	s.Intervention(t.Context(), "retry")
	out := scrape()
	for _, want := range []string{
		`agent_factory_pr_outcomes_total{class="docs-links",outcome="closed",repository="Smana/b"`,
		`agent_factory_pr_outcomes_total{class="docs-links",outcome="closed",repository="other"`,
		`agent_factory_time_to_pr_seconds_count{repository="Smana/b",source="issue",template="solo",tier="standard"}`,
		`agent_factory_human_interventions_total{kind="stop",repository="Smana/b"`,
		`agent_factory_human_interventions_total{kind="retry",repository="other"`,
		`agent_factory_intake_errors_total{repository="Smana/b",source="issue"`,
		`predicted_class="review",repository="Smana/a"`,
		`predicted_class="review",repository="other"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(out, "Evil/repo") || strings.Contains(out, "SMANA") {
		t.Fatal("a label value outside the list")
	}
}
