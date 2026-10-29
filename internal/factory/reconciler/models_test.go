// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"strings"
	"testing"

	v1alpha1 "github.com/Smana/maku/api/factory/v1alpha1"
	"github.com/Smana/maku/internal/factory/config"
	"github.com/Smana/maku/internal/factory/runs"
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

// F4 at the choke point: a task triaged public, then flipped internal with its provider judge,
// never gets a claim: runs.Spec.Validate refuses an internal run that names a provider alias.
func TestAnInternalRunWithAProviderJudgeIsRefused(t *testing.T) {
	g := newRig(t, issueTask("3buqdlot", 7, "x"))
	g.r.Cfg.Loops = map[string]config.Loop{"review": {Judge: "mistral-light"}}
	g.runs.refuse = func(s runs.Spec) error { return s.Validate() }
	tk := g.reconcile(t, "3buqdlot", 1)
	if tk.Status.Phase != v1alpha1.PhaseTriaged || tk.Spec.Loop == nil || tk.Spec.Loop.JudgeModel != "mistral-light" {
		t.Fatalf("setup: %s %+v", tk.Status.Phase, tk.Spec.Loop)
	}
	tk.Spec.DataClass = "internal"
	if err := g.c.Update(t.Context(), tk); err != nil {
		t.Fatal(err)
	}
	var last error
	for range 3 {
		if _, err := g.r.Reconcile(t.Context(), reqFor("3buqdlot")); err != nil {
			last = err
		}
	}
	if last == nil || !strings.Contains(last.Error(), "public") || len(g.runs.specs) != 0 {
		t.Fatalf("err %v, %d claims: want the run refused", last, len(g.runs.specs))
	}
}

// pinModel for every role that is not the implementer or the reviewer: the scoper (a triager on
// light), the tester (the task's tier) and investigate's triager. A public task takes the row's
// alias; an internal one keeps today's model, its source the tier.
func TestPinModelForEveryRole(t *testing.T) {
	row := map[string]map[string]string{
		"triager": {"light": "mistral-light", "standard": "mistral-standard"},
		"tester":  {"standard": "mistral-standard"},
	}
	for name, c := range map[string]struct {
		spec      func(*Reconciler, *v1alpha1.Task) runs.Spec
		dataClass string
		want      string
		source    string
	}{
		"scoper, public":                  {scoper, "public", "mistral-light", v1alpha1.ModelSourceClassModels},
		"scoper, internal":                {scoper, "internal", "agent-default", v1alpha1.ModelSourceTier},
		"tester, public":                  {tester, "public", "mistral-standard", v1alpha1.ModelSourceClassModels},
		"tester, internal":                {tester, "internal", "agent-default", v1alpha1.ModelSourceTier},
		"investigate's triager, public":   {investigator, "public", "mistral-standard", v1alpha1.ModelSourceClassModels},
		"investigate's triager, internal": {investigator, "internal", "agent-default", v1alpha1.ModelSourceTier},
	} {
		t.Run(name, func(t *testing.T) {
			g := newRig(t)
			g.r.Cfg.Tiers["light"] = config.Tier{Model: "agent-default", RunTokens: 300_000, TaskTokens: 600_000, RunMinutes: 20}
			g.r.Cfg.Loops = map[string]config.Loop{"review": {Models: row}}
			tk := issueTask("3buqdlot", 7, "x")
			tk.Spec.DataClass, tk.Spec.PredictedClass, tk.Spec.Template = c.dataClass, "review", "solo"
			tk.Spec.Budget = v1alpha1.Budget{Tier: "standard", Model: "agent-default", RunTokens: 1_500_000, RunMinutes: 45}
			s := c.spec(g.r, tk)
			s.RunID = rid(0)
			if got := g.r.pinModel(tk, &s); s.Model != c.want || got != c.source {
				t.Fatalf("%s %s: model %q source %q, want %q %q", s.Role, s.Tier, s.Model, got, c.want, c.source)
			}
			if err := s.Validate(); err != nil && c.dataClass == "public" {
				t.Fatalf("a public run on its alias is a valid claim: %v", err)
			}
		})
	}
}

func scoper(r *Reconciler, t *v1alpha1.Task) runs.Spec { return r.scoperSpec(t, "x") }
func tester(r *Reconciler, t *v1alpha1.Task) runs.Spec { return r.testerSpec(t, "x") }
func investigator(r *Reconciler, t *v1alpha1.Task) runs.Spec {
	s := r.implementerSpec(t, "x")
	s.Role = "triager"
	return s
}

// R48: a run adopted from its claim, its record lost, keeps the source of its model: derived from
// the claim's model, the role and the template.
func TestAnAdoptedRunKeepsItsModelSource(t *testing.T) {
	for name, c := range map[string]struct {
		x             runs.Run
		reviewerModel string
		want          string
	}{
		"a provider alias":     {runs.Run{ID: rid(0), TaskID: "3buqdlot", Role: "implementer", Model: "mistral-standard"}, "", v1alpha1.ModelSourceClassModels},
		"the tier's model":     {runs.Run{ID: rid(0), TaskID: "3buqdlot", Role: "implementer", Model: "agent-default"}, "", v1alpha1.ModelSourceTier},
		"the reviewerModel":    {runs.Run{ID: rid(0), TaskID: "3buqdlot", Role: "reviewer", Model: "tier-light"}, "tier-light", v1alpha1.ModelSourceReviewerModel},
		"a reviewer on a tier": {runs.Run{ID: rid(0), TaskID: "3buqdlot", Role: "reviewer", Model: "tier-frontier"}, "tier-light", v1alpha1.ModelSourceTier},
	} {
		t.Run(name, func(t *testing.T) {
			g := newRig(t)
			pair := g.r.Cfg.Templates["pair"]
			pair.ReviewerModel = c.reviewerModel
			g.r.Cfg.Templates["pair"] = pair
			tk := issueTask("3buqdlot", 7, "x")
			tk.Spec.Template = "pair"
			if err := g.r.recordExisting(tk, c.x); err != nil {
				t.Fatal(err)
			}
			if got := tk.Status.Runs[0].Provenance.ModelSource; got != c.want {
				t.Fatalf("model source %q, want %q", got, c.want)
			}
		})
	}
}
