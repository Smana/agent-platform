// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"testing"

	v1alpha1 "github.com/Smana/maku/api/factory/v1alpha1"
	"github.com/Smana/maku/internal/factory/config"
)

// v3 F15, F3: loops.<class>.models picks the run's alias for a public task, by role and the tier
// the run runs on; an internal task, and a role or tier without an entry, keep today's model. The
// run's provenance records which source chose it.
func TestClassModelsPickAPublicRunsAlias(t *testing.T) {
	models := map[string]map[string]string{
		"implementer": {"standard": "mistral-standard"},
		"reviewer":    {"frontier": "kimi-frontier"},
	}
	for name, c := range map[string]struct {
		dataClass, reviewerModel string
		models                   map[string]map[string]string
		implementer, reviewer    string
		implSource, revSource    string
	}{
		"public, both roles": {"public", "", models, "mistral-standard", "kimi-frontier",
			v1alpha1.ModelSourceClassModels, v1alpha1.ModelSourceClassModels},
		"internal: today's models": {"internal", "", models, "agent-default", "tier-frontier",
			v1alpha1.ModelSourceTier, v1alpha1.ModelSourceTier},
		"public, no reviewer entry: its reviewerModel": {"public", "tier-light",
			map[string]map[string]string{"implementer": {"standard": "mistral-standard"}}, "mistral-standard", "tier-light",
			v1alpha1.ModelSourceClassModels, v1alpha1.ModelSourceReviewerModel},
		"public, an entry wins over reviewerModel": {"public", "tier-light", models, "mistral-standard", "kimi-frontier",
			v1alpha1.ModelSourceClassModels, v1alpha1.ModelSourceClassModels},
		"public, no row: today's models": {"public", "", nil, "agent-default", "tier-frontier",
			v1alpha1.ModelSourceTier, v1alpha1.ModelSourceTier},
	} {
		t.Run(name, func(t *testing.T) {
			tk := issueTask("3buqdlot", 7, "x")
			tk.Spec.DataClass = c.dataClass
			g := teamRigFor(t, "pair", tk, func(g *rig) {
				if c.models != nil {
					g.r.Cfg.Loops = map[string]config.Loop{"review": {Models: c.models}}
				}
				pair := g.r.Cfg.Templates["pair"]
				pair.ReviewerModel = c.reviewerModel
				g.r.Cfg.Templates["pair"] = pair
			})
			impl, rev := g.runs.specs[rid(0)], g.runs.specs[rid(1)]
			if impl.Model != c.implementer || impl.Tier != "standard" {
				t.Fatalf("implementer %q on %q, want %q on standard", impl.Model, impl.Tier, c.implementer)
			}
			if rev.Model != c.reviewer || rev.Tier != "frontier" {
				t.Fatalf("reviewer %q on %q, want %q on frontier", rev.Model, rev.Tier, c.reviewer)
			}
			got := g.task(t)
			if s := got.Status.Runs[0].Provenance.ModelSource; s != c.implSource {
				t.Fatalf("implementer's model source %q, want %q", s, c.implSource)
			}
			if s := got.Status.Runs[1].Provenance.ModelSource; s != c.revSource {
				t.Fatalf("reviewer's model source %q, want %q", s, c.revSource)
			}
		})
	}
}

// F4: the judge's alias is a provider's only on a public task; an internal task of the class gets
// the default judge, never mistral-light.
func TestAProviderJudgeIsPublicOnly(t *testing.T) {
	for class, want := range map[string]string{"public": "mistral-light", "internal": config.DefaultJudge} {
		t.Run(class, func(t *testing.T) {
			tk := issueTask("3buqdlot", 7, "x")
			tk.Spec.DataClass = class
			g := newRig(t, tk)
			g.r.Cfg.Loops = map[string]config.Loop{"review": {Judge: "mistral-light"}}
			got := g.reconcile(t, "3buqdlot", 3)
			if got.Spec.Loop == nil || got.Spec.Loop.JudgeModel != want {
				t.Fatalf("judge %+v, want %q", got.Spec.Loop, want)
			}
		})
	}
}
