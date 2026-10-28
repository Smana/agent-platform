// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/api"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/forge"
	"github.com/Smana/agent-platform/internal/factory/triage"
)

// Offline proof 4, end to end: the tester's journeys through a real API server, which holds
// status.contract to its CRD bounds and refuses a status write the fake client would take. Each journey
// runs the reconciler's own passes against the fake forge, room and runs, on envtest's API server.

// envTesterRig is testerRigCfg on a real API server, with a clock the test moves.
func envTesterRig(t *testing.T, edit func(*v1alpha1.Task), cfgEdit func(*config.Config)) (*rig, *time.Time) {
	t.Helper()
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("KUBEBUILDER_ASSETS unset: run `task test`, which sets it")
	}
	env := &envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "..", "config", "crd")}, ErrorIfCRDPathMissing: true}
	restCfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = env.Stop() })
	c, err := client.New(restCfg, client.Options{Scheme: scheme()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "agent-system"}}); err != nil {
		t.Fatal(err)
	}
	clock := time.Now()
	if err := api.StampMeter(ctx, c, "agent-system", clock, 1000*time.Hour); err != nil {
		t.Fatal(err)
	}
	tk := issueTask("3buqdlot", 7, "# Add --dry-run to the script\n\n## Acceptance check\n`scripts/ci/tests/run.sh` passes")
	tk.CreationTimestamp = metav1.Time{}
	if edit != nil {
		edit(tk)
	}
	g := newRig(t) // the fake client it builds is replaced below
	g.c, g.r.Client, g.r.Ledger = c, c, c
	g.r.Now = func() time.Time { return clock }
	g.r.Cfg.AgentsLogin = agentsApp
	g.r.Cfg.Defaults.PredictedClass = "scripts"
	g.r.Cfg.Merge.ContractCheck = contractCheck
	g.r.Cfg.Classes["scripts"] = config.Class{Checks: true, TestFirst: true, Live: true}
	if cfgEdit != nil {
		cfgEdit(g.r.Cfg)
	}
	g.r.Triage = triage.Static{Cfg: g.r.Cfg}
	if err := c.Create(ctx, tk); err != nil {
		t.Fatal(err)
	}
	g.reconcile(t, "3buqdlot", 3) // Received → Triaged → Queued → the first run
	return g, &clock
}

// contractAndImplementer ends the tester run with a recorded contract push and reconciles the implementer's start.
func (g *rig) contractAndImplementer(t *testing.T) *v1alpha1.Task {
	t.Helper()
	g.pushed(agentsApp)
	g.contractIs(contractSHA, rid(0), []string{"scripts/ci/tests/test_dry_run.py"}, []string{"a::one", "a::two"}, true)
	g.harnessNote(rid(0), "Harness (tester): contract_ready: 2 tests, red on the current code")
	g.finish(rid(0), "Succeeded", "agent_finished")
	return g.reconcile(t, "3buqdlot", 2)
}

// implementerOpensThePR is the implementer rid(n)'s pull request landing on the contract: its push on
// top of the tester's creation, its handoff, its end.
func (g *rig) implementerOpensThePR(t *testing.T, n int) *v1alpha1.Task {
	t.Helper()
	g.f.SetBranch("agent/3buqdlot", 12)
	g.f.SetPR(forge.PR{Number: 12, NodeID: "PR_12", URL: "https://github.com/Smana/cloud-native-ref/pull/12", State: "OPEN", Author: agentsApp,
		HeadRef: "agent/3buqdlot", HeadSHA: head1, HeadMessage: "feat: dry run\n\nAgent-Run: " + rid(n)})
	g.f.SetActivity("agent/3buqdlot",
		forge.Push{Type: "push", Actor: agentsApp, Before: contractSHA, After: head1},
		forge.Push{Type: "branch_creation", Actor: agentsApp, Before: strings.Repeat("0", 40), After: contractSHA})
	g.log.handoff(rid(n), head1)
	g.finish(rid(n), "Succeeded", "agent_finished")
	return g.reconcile(t, "3buqdlot", 3)
}

// 1. testFirst off: a pair task's journey is the same with or without the keys, role for role.
func TestEnvtestTestFirstOffLeavesThePairJourneyUnchanged(t *testing.T) {
	journey := func(cfgEdit func(*config.Config)) string {
		g, _ := envTesterRig(t, nil, func(c *config.Config) {
			c.Defaults.PredictedClass = "review"
			c.Defaults.Template = "pair"
			if cfgEdit != nil {
				cfgEdit(c)
			}
		})
		g.f.SetBranch("agent/3buqdlot", 12)
		g.f.SetPR(pr12At(head1))
		g.finish(rid(0), "Succeeded", "agent_finished")
		g.reconcile(t, "3buqdlot", 2) // the reviewer rid(1)
		g.log.verdict(rid(1), "approve", head1, "LGTM")
		g.finish(rid(1), "Succeeded", "agent_finished")
		tk := g.reconcile(t, "3buqdlot", 2)
		var seq []string
		for _, r := range tk.Status.Runs {
			seq = append(seq, r.Role+"/"+r.Trigger+"/"+r.Phase)
		}
		return fmt.Sprintf("%s %q %s rounds=%d contract=%v runs=%v", tk.Status.Phase, tk.Status.Reason, tk.Status.Verdict,
			tk.Status.ReviewRounds, tk.Status.Contract, seq)
	}
	without := journey(func(c *config.Config) { delete(c.Classes, "scripts"); c.Merge.ContractCheck = "" })
	with := journey(nil) // scripts declares testFirst, and this task is of the review class
	if without != with || !strings.Contains(with, "AwaitingCI") || strings.Contains(with, "tester") {
		t.Fatalf("with the keys:    %s\nwithout the keys: %s", with, without)
	}
}

// 2. On: the tester's contract, the implementer held to it, and rows 2b and 2c at the gate.
func TestEnvtestATestFirstTaskJourneyThroughRows2bAnd2c(t *testing.T) {
	soloScripts := func(c *config.Config) { c.Defaults.Template = "solo" }
	type outcome struct {
		name    string
		checks  forge.Checks
		changed []string
		phase   string
		reason  string
	}
	for _, o := range []outcome{
		{"the contract check pending waits", withContractCheck("PENDING"), nil, v1alpha1.PhaseAwaitingCI, "ci_pending"},
		{"a red contract check starts a fix run naming it", withContractCheck("FAILURE"), nil, v1alpha1.PhaseImplementing, ""},
		{"green with a changed contract file is a human's", withContractCheck("SUCCESS"), []string{"scripts/ci/tests/test_dry_run.py"},
			v1alpha1.PhaseAwaitingHuman, "contract_modified"},
		{"green and unchanged is the class rows'", withContractCheck("SUCCESS"), []string{"scripts/other.sh"}, v1alpha1.PhaseAutoMerging, ""},
	} {
		t.Run(o.name, func(t *testing.T) {
			g, _ := envTesterRig(t, nil, soloScripts)
			if s := g.runs.specs[rid(0)]; s.Role != "tester" {
				t.Fatalf("first run %s", s.Role)
			}
			tk := g.contractAndImplementer(t)
			want := &v1alpha1.ContractStatus{RunID: rid(0), Commit: contractSHA, Files: []string{"scripts/ci/tests/test_dry_run.py"}, IDs: []string{"a::one", "a::two"}}
			if !reflect.DeepEqual(tk.Status.Contract, want) || g.runs.specs[rid(1)].Role != "implementer" {
				t.Fatalf("contract %+v, run 1 %s", tk.Status.Contract, g.runs.specs[rid(1)].Role)
			}
			for _, want := range []string{contractSHA[:12], "scripts/ci/tests/test_dry_run.py", "a::one", "a::two", "CONTRACT-n0nce234"} {
				if !strings.Contains(g.runs.specs[rid(1)].TaskText, want) {
					t.Errorf("the implementer's claim lacks %q", want)
				}
			}
			tk = g.implementerOpensThePR(t, 1)
			if tk.Status.Phase != v1alpha1.PhaseAwaitingCI {
				t.Fatalf("%s %q: the pull request reaches the gate", tk.Status.Phase, tk.Status.Reason)
			}
			g.f.SetChecks(12, o.checks)
			g.f.SetChangedFiles(contractSHA, head1, o.changed, nil)
			tk = g.reconcile(t, "3buqdlot", 1)
			if o.phase == v1alpha1.PhaseImplementing {
				tk = g.reconcile(t, "3buqdlot", 1) // Queued → the fix run
			}
			if tk.Status.Phase != o.phase || (o.reason != "" && tk.Status.Reason != o.reason) {
				t.Fatalf("%s %q, want %s %q", tk.Status.Phase, tk.Status.Reason, o.phase, o.reason)
			}
			if o.phase == v1alpha1.PhaseImplementing {
				if tk.Status.FixRuns != 1 || !strings.Contains(g.runs.specs[rid(2)].TaskText, contractCheck) || !strings.Contains(g.runs.specs[rid(2)].TaskText, "The test contract") {
					t.Fatalf("fix runs %d:\n%s", tk.Status.FixRuns, g.runs.specs[rid(2)].TaskText)
				}
			}
		})
	}
}

// 3. On, and the tester leaves nothing: the implementer runs without a contract and the contract check
// is not required, a red one arms nothing differently.
func TestEnvtestATesterThatLeavesNothingRequiresNoContractCheck(t *testing.T) {
	g, _ := envTesterRig(t, nil, func(c *config.Config) { c.Defaults.Template = "solo" })
	g.harnessNote(rid(0), "Harness (tester): not_testable: a doc-only change")
	g.finish(rid(0), "Succeeded", "agent_finished")
	tk := g.reconcile(t, "3buqdlot", 2)
	if tk.Status.Contract != nil || g.runs.specs[rid(1)].Role != "implementer" || strings.Contains(g.runs.specs[rid(1)].TaskText, "The test contract") {
		t.Fatalf("contract %+v, run 1 %+v", tk.Status.Contract, g.runs.specs[rid(1)])
	}
	g.f.SetBranch("agent/3buqdlot", 12)
	g.f.SetPR(forge.PR{Number: 12, NodeID: "PR_12", URL: "https://github.com/Smana/cloud-native-ref/pull/12", State: "OPEN", Author: agentsApp,
		HeadRef: "agent/3buqdlot", HeadSHA: head1, HeadMessage: "feat: dry run\n\nAgent-Run: " + rid(1)})
	g.log.handoff(rid(1), head1)
	g.finish(rid(1), "Succeeded", "agent_finished")
	g.reconcile(t, "3buqdlot", 3)
	g.f.SetChecks(12, withContractCheck("FAILURE")) // the job only reports on a task without a contract (TF7)
	if tk = g.reconcile(t, "3buqdlot", 1); tk.Status.Phase != v1alpha1.PhaseAutoMerging || tk.Status.FixRuns != 0 {
		t.Fatalf("%s %q fixes %d", tk.Status.Phase, tk.Status.Reason, tk.Status.FixRuns)
	}
}

// 4. A shadow class with holdoutChecks and a contract: rows 2b and 2c decide before holdout_pending.
func TestEnvtestRows2bAnd2cDecideBeforeTheHoldouts(t *testing.T) {
	g, _ := envTesterRig(t, nil, func(c *config.Config) {
		c.Defaults.Template = "solo"
		c.Classes["scripts"] = config.Class{Checks: true, TestFirst: true, Shadow: true, HoldoutChecks: []string{"Holdout 🧪"}}
	})
	g.contractAndImplementer(t)
	if tk := g.implementerOpensThePR(t, 1); tk.Status.Phase != v1alpha1.PhaseAwaitingCI {
		t.Fatalf("%s %q", tk.Status.Phase, tk.Status.Reason)
	}
	pending := withContractCheck("PENDING")
	g.f.SetChecks(12, pending)
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Reason != "ci_pending" { // 2b: before any holdout
		t.Fatalf("%s %q", tk.Status.Phase, tk.Status.Reason)
	}
	g.f.SetChecks(12, withContractCheck("SUCCESS")) // the holdout never ran: pending
	g.f.SetChangedFiles(contractSHA, head1, nil, nil)
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Reason != "holdout_pending" {
		t.Fatalf("%s %q: with the contract intact the holdouts are next", tk.Status.Phase, tk.Status.Reason)
	}
	g.f.SetChangedFiles(contractSHA, head1, []string{"scripts/ci/tests/test_dry_run.py"}, nil)
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Phase != v1alpha1.PhaseAwaitingHuman || tk.Status.Reason != "contract_modified" { // 2c: before the holdouts
		t.Fatalf("%s %q", tk.Status.Phase, tk.Status.Reason)
	}
}
