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
		`agent_factory_pr_outcomes_total{class="docs-links",outcome="closed",principal="other",repository="Smana/b"`,
		`agent_factory_pr_outcomes_total{class="docs-links",outcome="closed",principal="other",repository="other"`,
		`agent_factory_time_to_pr_seconds_count{repository="Smana/b",source="issue",template="solo",tier="standard"}`,
		`agent_factory_human_interventions_total{kind="stop",repository="Smana/b"`,
		`agent_factory_human_interventions_total{kind="retry",repository="other"`,
		`agent_factory_intake_errors_total{repository="Smana/b",source="issue"`,
		`predicted_class="review",principal="system:factory",repository="Smana/a"`,
		`predicted_class="review",principal="system:factory",repository="other"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(out, "Evil/repo") || strings.Contains(out, "SMANA") {
		t.Fatal("a label value outside the list")
	}
}

// v2 D7 and the admission paths: one gauge per listed repository, and refusals counted by
// repository and reason; a reason outside the repository codes is not counted here.
func TestOwnershipAndAdmissionRefusals(t *testing.T) {
	s, scrape := rig(t, nil, func() bool { return true })
	s.ListRepositories("Smana/a", "Smana/b")
	s.RepositoryOwned(t.Context(), "Smana/a", true)
	s.RepositoryOwned(t.Context(), "smana/b", false)
	s.AdmissionRefused(WithRepository(t.Context(), "Smana/b"), "repository_not_owned")
	s.AdmissionRefused(WithRepository(t.Context(), "Smana/a"), "repository_permission")
	s.AdmissionRefused(WithRepository(t.Context(), "Smana/a"), "room_busy")
	out := scrape()
	for _, want := range []string{
		`agent_factory_repository_owned{repository="Smana/a"} 1`,
		`agent_factory_repository_owned{repository="Smana/b"} 0`,
		`agent_factory_admission_refusals_total{principal="other",reason="repository_not_owned",repository="Smana/b"} 1`,
		`agent_factory_admission_refusals_total{principal="other",reason="repository_permission",repository="Smana/a"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(out, "room_busy") {
		t.Fatal("only the repository refusals are counted")
	}
}
