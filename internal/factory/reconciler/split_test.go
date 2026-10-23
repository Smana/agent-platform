// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/triage"
)

// c7 is triage answering tier as C7 would, the control group forcing frontier: classification.tier
// stays C7's own verdict (OD-14).
type c7 struct {
	cfg     *config.Config
	tier    string
	control bool
}

func (c c7) Triage(_ context.Context, t *v1alpha1.Task) (triage.Decision, error) {
	acting := c.tier
	if c.control {
		acting = "frontier"
	}
	return triage.Decision{Template: triage.Matrix("review", "issue", acting), PredictedClass: "review", DataClass: t.Spec.DataClass,
		Budget:         triage.BudgetFor(c.cfg, acting),
		Classification: v1alpha1.Classification{Tier: c.tier, Classifier: "c7", Fallback: "none", Control: c.control}}, nil
}

// parentText is the parent issue as labelled: the whole task, with its own acceptance section.
const parentText = "# Fix both links to runbook 07\n\n## Acceptance check\n- the whole issue's check"

// splitRigWith is the rig with the host's USD tiers, loops.review {scope: scoper, split:
// {maxChildren: 3}} and C7 answering tier; edit shapes the task before its triage.
func splitRigWith(t *testing.T, tier string, control bool, edit func(*v1alpha1.Task), objs ...client.Object) *rig {
	t.Helper()
	tk := issueTask("3buqdlot", 7, parentText)
	if edit != nil {
		edit(tk)
	}
	g := newRig(t, append([]client.Object{tk}, objs...)...)
	c := g.r.Cfg
	c.Tiers["light"] = config.Tier{Model: "agent-default", RunTokens: 300_000, TaskTokens: 600_000, RunMinutes: 20, RunUSD: 0.51, TaskUSD: 1.02}
	for name, usd := range map[string][2]float64{"standard": {2.55, 5.10}, "frontier": {6.80, 13.60}} {
		tr := c.Tiers[name]
		tr.RunUSD, tr.TaskUSD = usd[0], usd[1]
		c.Tiers[name] = tr
	}
	c.Loops = map[string]config.Loop{"review": {Scope: config.ScopeScoper, Split: &config.SplitPolicy{MaxChildren: 3}}}
	g.r.Triage = c7{cfg: c, tier: tier, control: control}
	return g
}

func splitRig(t *testing.T, tier string, control bool) *rig {
	return splitRigWith(t, tier, control, nil)
}

// Splitter design SL3, SP1, offline proof 3: spec.split is set on C7's own frontier verdict only, a
// control-forced frontier included out; the parent's own budget stays its tier's (SR3).
func TestTriageOffersASplitOnC7sOwnFrontier(t *testing.T) {
	for name, c := range map[string]struct {
		tier    string
		control bool
		offered bool
	}{
		"C7 frontier":                      {"frontier", false, true},
		"C7 frontier in the control group": {"frontier", true, true},
		"a control-forced frontier":        {"standard", true, false},
		"C7 standard":                      {"standard", false, false},
	} {
		t.Run(name, func(t *testing.T) {
			tk := splitRig(t, c.tier, c.control).reconcile(t, "3buqdlot", 1)
			if (tk.Spec.Split != nil) != c.offered {
				t.Fatalf("split %+v", tk.Spec.Split)
			}
			if c.offered && *tk.Spec.Split != (v1alpha1.SplitSpec{MaxChildren: 3, FamilyMicroUSD: 20_400_000, ChildTier: "standard"}) {
				t.Fatalf("split %+v", tk.Spec.Split)
			}
			if c.offered && tk.Spec.Budget.TaskMicroUSD != 13_600_000 {
				t.Fatalf("the parent's cap is its tier's: %d", tk.Spec.Budget.TaskMicroUSD)
			}
		})
	}
}

// No row, a row without split, a mode other than scoper, or no issue: no split.
func TestNoSplitOutsideItsRow(t *testing.T) {
	for name, edit := range map[string]func(*rig){
		"a scoper row without split": func(g *rig) { g.r.Cfg.Loops = map[string]config.Loop{"review": {Scope: config.ScopeScoper}} },
		"a steering row with split": func(g *rig) {
			g.r.Cfg.Loops = map[string]config.Loop{"review": {Scope: config.ScopeSteering, Split: &config.SplitPolicy{MaxChildren: 3}}}
		},
		"no loops": func(g *rig) { g.r.Cfg.Loops = nil },
	} {
		t.Run(name, func(t *testing.T) {
			g := splitRig(t, "frontier", false)
			edit(g)
			if tk := g.reconcile(t, "3buqdlot", 1); tk.Spec.Split != nil {
				t.Fatalf("split %+v", tk.Spec.Split)
			}
		})
	}
	g := splitRigWith(t, "frontier", false, func(tk *v1alpha1.Task) { tk.Spec.Issue, tk.Spec.Source.Kind = 0, "schedule" })
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Spec.Split != nil {
		t.Fatal("a task without an issue has nowhere to propose")
	}
}

// Offline proof 7, the scoper's half: the split clause is in the scoping brief only when offered.
func TestTheScoperIsToldItMaySplit(t *testing.T) {
	tk := issueTask("3buqdlot", 7, parentText)
	if b := ScoperBrief(tk, "n0nce234"); strings.Contains(b, "split") {
		t.Fatalf("a scoper not offered a split hears of one:\n%s", b)
	}
	tk.Spec.Split = &v1alpha1.SplitSpec{MaxChildren: 3, FamilyMicroUSD: 20_400_000, ChildTier: "standard"}
	for _, want := range []string{"2 to 3 changes", "hand off split {why, children}", "beside scope", "in the order they merge"} {
		if b := ScoperBrief(tk, "n0nce234"); !strings.Contains(b, want) {
			t.Errorf("the split clause lacks %q", want)
		}
	}
}
