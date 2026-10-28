// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/triage"
)

// testerRig is a public scripts task whose class switches testFirst on, triaged and queued once.
func testerRig(t *testing.T, edit func(*v1alpha1.Task)) *rig { return testerRigCfg(t, edit, nil) }

// testerRigCfg is testerRig with a hook on the config before the task's triage.
func testerRigCfg(t *testing.T, edit func(*v1alpha1.Task), cfgEdit func(*config.Config)) *rig {
	t.Helper()
	tk := issueTask("3buqdlot", 7, "# Add --dry-run to the script\n\n## Acceptance check\n`scripts/ci/tests/run.sh` passes")
	tk.Spec.PredictedClass = "scripts"
	if edit != nil {
		edit(tk)
	}
	g := newRig(t, tk)
	g.r.Cfg.Classes["scripts"] = config.Class{Checks: true, TestFirst: true}
	g.r.Cfg.Merge.ContractCheck = "Test contract 🧪"
	g.r.Cfg.Defaults.PredictedClass = tk.Spec.PredictedClass // the static triage's class
	g.r.Triage = triage.Static{Cfg: g.r.Cfg}
	if cfgEdit != nil {
		cfgEdit(g.r.Cfg)
	}
	g.reconcile(t, "3buqdlot", 3)
	return g
}

// task reads the rig's task as it now stands.
func (g *rig) task(t *testing.T) *v1alpha1.Task {
	t.Helper()
	var tk v1alpha1.Task
	if err := g.c.Get(t.Context(), types.NamespacedName{Namespace: "agent-system", Name: "3buqdlot"}, &tk); err != nil {
		t.Fatal(err)
	}
	return &tk
}

func TestATestFirstClassStartsWithTheTester(t *testing.T) {
	g := testerRig(t, nil)
	s := g.runs.specs[rid(0)]
	if s.Role != "tester" || s.Branch != "agent/3buqdlot" || s.BaseRef != "main" || !s.Checks || s.TaskURL != "" {
		t.Fatalf("the tester's claim %+v", s)
	}
	if s.Model != g.task(t).Spec.Budget.Model || s.MaxTokens != g.task(t).Spec.Budget.RunTokens {
		t.Fatalf("the tester runs on the implementer's spec (R19): %+v", s)
	}
	for _, want := range []string{"Write failing tests", "never push", "TASK-DATA-"} {
		if !strings.Contains(s.TaskText, want) {
			t.Errorf("the tester's brief lacks %q", want)
		}
	}
	if g.task(t).Status.Phase != v1alpha1.PhaseImplementing {
		t.Fatalf("phase %s: the tester is a writer (TF1)", g.task(t).Status.Phase)
	}
	if s.Loop != nil || s.Search {
		t.Fatalf("the loop policy and search close implementer runs only: %+v", s)
	}
}

func TestTestFirstOffOrAnInternalTaskRunsTodaysSequence(t *testing.T) {
	for name, edit := range map[string]func(*v1alpha1.Task){
		"internal": func(tk *v1alpha1.Task) { tk.Spec.DataClass = "internal" },
		"docs":     func(tk *v1alpha1.Task) { tk.Spec.PredictedClass = "docs" },
	} {
		if s := testerRig(t, edit).runs.specs[rid(0)]; s.Role != "implementer" {
			t.Errorf("%s: first run %s, want the implementer", name, s.Role)
		}
	}
}

func TestTheScoperRunsBeforeTheTester(t *testing.T) {
	g := testerRigCfg(t, nil, func(c *config.Config) {
		c.Loops = map[string]config.Loop{"scripts": {Scope: config.ScopeScoper}}
		c.Tiers["light"] = config.Tier{Model: "agent-default", RunTokens: 300_000, TaskTokens: 600_000, RunMinutes: 20}
	})
	if s := g.runs.specs[rid(0)]; s.Role != "triager" {
		t.Fatalf("first run %s, want the scoper", s.Role)
	}
	scopeHandoff(g, 0, readyScope())
	endScoper(t, g, "Succeeded", "agent_finished")
	g.reconcile(t, "3buqdlot", 1)
	s := g.runs.specs[rid(1)]
	if s.Role != "tester" || !strings.Contains(s.TaskText, "Point the link at runbook 08.") {
		t.Fatalf("second run %s: the tester's brief carries the scope\n%s", s.Role, s.TaskText)
	}
}

func TestALostTesterResumesAsATester(t *testing.T) {
	g := testerRig(t, nil)
	g.runs.lose(rid(0))
	g.log.end(rid(0), "Failed", "pod_lost")
	tk := g.reconcile(t, "3buqdlot", 2)
	s := g.runs.specs[rid(1)]
	if s.Role != "tester" || len(tk.Status.Runs) != 2 || tk.Status.Runs[1].Trigger != "resume" {
		t.Fatalf("%s %+v", s.Role, tk.Status.Runs)
	}
	if !strings.Contains(s.TaskText, "interrupted by the platform") || strings.Contains(s.TaskText, "What it pushed") {
		t.Fatalf("the resumed tester's notice:\n%s", s.TaskText)
	}
}
