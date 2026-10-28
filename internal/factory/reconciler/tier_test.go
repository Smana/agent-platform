// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"testing"

	v1alpha1 "github.com/Smana/maku/api/factory/v1alpha1"
	"github.com/Smana/maku/internal/factory/config"
)

// R47: the tier on the claim is the tier the run runs on, fixed for the run.
func TestAReviewerCarriesTheTierItRunsOn(t *testing.T) {
	g := newRig(t)
	g.r.Cfg.Tiers["frontier"] = config.Tier{Model: "agent-default", RunTokens: 4_000_000, TaskTokens: 8_000_000, RunMinutes: 90}
	tk := issueTask("3buqdlot", 7, "fix")
	tk.Spec.Budget = v1alpha1.Budget{Tier: "standard", Model: "agent-default", RunTokens: 1_500_000, RunMinutes: 45}
	tk.Status.PullRequest = &v1alpha1.PullRequestRef{Number: 12, URL: "https://github.com/Smana/cloud-native-ref/pull/12"}
	if s := g.r.verifierSpec(tk, "reviewer"); s.Tier != "frontier" {
		t.Fatalf("reviewer tier %q, want frontier", s.Tier)
	}
	if s := g.r.verifierSpec(tk, "tester"); s.Tier != "standard" {
		t.Fatalf("tester tier %q, want standard", s.Tier)
	}
}

// A template's reviewerModel picks the reviewer's model apart from the implementer's tier; the run
// keeps the other tier's caps. Without it the reviewer takes that tier's model, as before.
func TestATemplatesReviewerModelOverridesTheOtherTiersModel(t *testing.T) {
	g := newRig(t)
	tk := issueTask("3buqdlou", 7, "fix")
	tk.Spec.Template = "pair"
	tk.Spec.Budget = v1alpha1.Budget{Tier: "standard", Model: "agent-default", RunTokens: 1_500_000, RunMinutes: 45}
	tk.Status.PullRequest = &v1alpha1.PullRequestRef{Number: 12, URL: "https://github.com/Smana/cloud-native-ref/pull/12"}
	if s := g.r.verifierSpec(tk, "reviewer"); s.Model != "tier-frontier" {
		t.Fatalf("default reviewer model %q, want the frontier tier's tier-frontier", s.Model)
	}
	pair := g.r.Cfg.Templates["pair"]
	pair.ReviewerModel = "tier-light"
	g.r.Cfg.Templates["pair"] = pair
	if s := g.r.verifierSpec(tk, "reviewer"); s.Model != "tier-light" || s.Tier != "frontier" {
		t.Fatalf("reviewer model %q tier %q, want tier-light on frontier", s.Model, s.Tier)
	}
	if s := g.r.verifierSpec(tk, "tester"); s.Model != "agent-default" {
		t.Fatalf("tester model %q, want the implementer's agent-default", s.Model)
	}
}
