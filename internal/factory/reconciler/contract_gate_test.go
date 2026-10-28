// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"errors"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/forge"
)

const contractCheck = "Test contract 🧪"

// contractOf is a contract of one file, as the tester's push recorded it.
func contractOf() *v1alpha1.ContractStatus {
	return &v1alpha1.ContractStatus{RunID: "7f3cq2xz", Commit: strings.Repeat("c", 40),
		Files: []string{"scripts/ci/tests/test_a.py"}, IDs: []string{"a::one"}}
}

// withContractCheck is green with the contract check reporting state.
func withContractCheck(state string) forge.Checks {
	c := green("SUCCESS")
	c.Runs = append(c.Runs, forge.Check{Name: contractCheck, State: state})
	return c
}

// Row 2b (R21, F3-R8): the contract check is required on a task with a contract, and only reported on
// one without (TF7).
func TestRow2bTheContractCheckIsRequiredOnATaskWithAContract(t *testing.T) {
	cfg := armCfg()
	cfg.Merge.ContractCheck = contractCheck
	with, without := armTask("docs-links", "solo", ""), armTask("docs-links", "solo", "")
	with.Status.Contract = contractOf()
	missing := green("SUCCESS")
	for name, c := range map[string]struct {
		task            *v1alpha1.Task
		checks          forge.Checks
		verdict, reason string
	}{
		"missing":                    {with, missing, "wait", "ci_pending"},
		"pending":                    {with, withContractCheck("PENDING"), "wait", "ci_pending"},
		"red":                        {with, withContractCheck("FAILURE"), "ci_red", "ci_red"},
		"green":                      {with, withContractCheck("SUCCESS"), "arm", ""},
		"red, no contract (TF7)":     {without, withContractCheck("FAILURE"), "arm", ""},
		"missing, no contract (TF7)": {without, missing, "arm", ""},
	} {
		d := DecideArm(ArmInputs{PR: agentPR(), Checks: c.checks, Task: c.task, Cfg: cfg, ReportedHead: head,
			Files: changed("[x](old.md)", "[x](new.md)"), Contract: ContractRead{Read: true}})
		if d.Verdict != c.verdict || d.Reason != c.reason {
			t.Errorf("%s: got %s/%s, want %s/%s", name, d.Verdict, d.Reason, c.verdict, c.reason)
		}
	}
}

func contractMergeRig(t *testing.T) *rig {
	t.Helper()
	g := mergeRig(t, v1alpha1.PhaseAwaitingCI)
	g.r.Cfg.Merge.ContractCheck = contractCheck
	var tk v1alpha1.Task
	_ = g.c.Get(t.Context(), client.ObjectKey{Namespace: "agent-system", Name: "3buqdlot"}, &tk)
	tk.Status.Contract = contractOf()
	if err := g.c.Status().Update(t.Context(), &tk); err != nil {
		t.Fatal(err)
	}
	return g
}

func TestAFixRunNamesARedContractCheck(t *testing.T) {
	g := contractMergeRig(t)
	g.f.SetChecks(12, withContractCheck("FAILURE"))
	tk := g.reconcile(t, "3buqdlot", 2)
	if tk.Status.Phase != v1alpha1.PhaseImplementing || tk.Status.FixRuns != 1 || !strings.Contains(g.runs.specs[rid(1)].TaskText, contractCheck) {
		t.Fatalf("%s fixes %d", tk.Status.Phase, tk.Status.FixRuns)
	}
	if c := strings.Join(g.f.Comments(7), "\n"); !strings.Contains(c, "`"+contractCheck+"`: failure") {
		t.Fatalf("the narration: %q", c)
	}
}

func TestUnreportedListsThePendingContractCheck(t *testing.T) {
	cfg := armCfg()
	cfg.Merge.ContractCheck = contractCheck
	tk := armTask("docs-links", "solo", "")
	in := ArmInputs{Checks: green("SUCCESS"), Task: tk, Cfg: cfg}
	if got := unreported(in, cfg.Classes["docs-links"]); len(got) != 0 {
		t.Fatalf("no contract: %v", got)
	}
	tk.Status.Contract = contractOf()
	if got := unreported(in, cfg.Classes["docs-links"]); len(got) != 1 || got[0] != contractCheck {
		t.Fatalf("with a contract: %v", got)
	}
}

// Row 2c (R21, F3-R9): once CI is green, a contract file among the changed files is a human's, and an
// unread list never passes.
func TestRow2cAChangedContractFileGoesToAHuman(t *testing.T) {
	cfg := armCfg()
	cfg.Merge.ContractCheck = contractCheck
	with := armTask("docs-links", "solo", "")
	with.Status.Contract = contractOf()
	without := armTask("docs-links", "solo", "")
	for name, c := range map[string]struct {
		task            *v1alpha1.Task
		read            ContractRead
		verdict, reason string
	}{
		"unread":                     {with, ContractRead{}, "wait", "contract_unread"},
		"a contract file changed":    {with, ContractRead{Read: true, Changed: []string{"x.py", "scripts/ci/tests/test_a.py"}}, "human", "contract_modified"},
		"other files only":           {with, ContractRead{Read: true, Changed: []string{"scripts/a.sh"}}, "arm", ""},
		"nothing changed":            {with, ContractRead{Read: true}, "arm", ""},
		"no contract, whatever read": {without, ContractRead{Changed: []string{"scripts/ci/tests/test_a.py"}}, "arm", ""},
	} {
		d := DecideArm(ArmInputs{PR: agentPR(), Checks: withContractCheck("SUCCESS"), Task: c.task, Cfg: cfg, ReportedHead: head,
			Files: changed("[x](old.md)", "[x](new.md)"), Contract: c.read})
		if d.Verdict != c.verdict || d.Reason != c.reason {
			t.Errorf("%s: got %s/%s, want %s/%s", name, d.Verdict, d.Reason, c.verdict, c.reason)
		}
	}
	// A red CI outranks the row: the contract's files are read once CI is green.
	d := DecideArm(ArmInputs{PR: agentPR(), Checks: withContractCheck("FAILURE"), Task: with, Cfg: cfg, ReportedHead: head})
	if d.Verdict != "ci_red" {
		t.Errorf("red: %+v", d)
	}
}

func changedFilesCalls(g *rig) int {
	n := 0
	for _, c := range g.f.Calls() {
		if strings.HasPrefix(c, "ChangedFiles ") {
			n++
		}
	}
	return n
}

func TestAwaitingCIReadsTheChangedFilesOncePerDecision(t *testing.T) {
	commit := contractOf().Commit
	// CI not green: no read.
	g := contractMergeRig(t)
	g.f.SetChecks(12, withContractCheck("PENDING"))
	g.reconcile(t, "3buqdlot", 1)
	if n := changedFilesCalls(g); n != 0 {
		t.Fatalf("%d reads while CI is pending", n)
	}
	// Green, a contract file changed: one read, a human.
	g = contractMergeRig(t)
	g.f.SetChecks(12, withContractCheck("SUCCESS"))
	g.f.SetChangedFiles(commit, "abc", []string{"scripts/ci/tests/test_a.py"}, nil)
	tk := g.reconcile(t, "3buqdlot", 1)
	if tk.Status.Phase != v1alpha1.PhaseAwaitingHuman || tk.Status.Reason != "contract_modified" || changedFilesCalls(g) != 1 {
		t.Fatalf("%s %q, %d reads", tk.Status.Phase, tk.Status.Reason, changedFilesCalls(g))
	}
	// Green, other files only: the class rows decide (docs-links is live in the rig).
	g = contractMergeRig(t)
	g.f.SetChecks(12, withContractCheck("SUCCESS"))
	g.f.SetChangedFiles(commit, "abc", []string{"scripts/a.sh"}, nil)
	if tk = g.reconcile(t, "3buqdlot", 1); strings.HasPrefix(tk.Status.Reason, "contract_") || changedFilesCalls(g) != 1 {
		t.Fatalf("%s %q, %d reads", tk.Status.Phase, tk.Status.Reason, changedFilesCalls(g))
	}
	// A rebase: not a descendant, every contract file counts as changed.
	g = contractMergeRig(t)
	g.f.SetChecks(12, withContractCheck("SUCCESS"))
	g.f.SetChangedFiles(commit, "abc", nil, forge.ErrNotDescendant)
	if tk = g.reconcile(t, "3buqdlot", 1); tk.Status.Reason != "contract_modified" {
		t.Fatalf("%s %q", tk.Status.Phase, tk.Status.Reason)
	}
	// Any other error: the pass retries, never a pass.
	g = contractMergeRig(t)
	g.f.SetChecks(12, withContractCheck("SUCCESS"))
	g.f.SetChangedFiles(commit, "abc", nil, errors.New("boom"))
	if _, err := g.r.Reconcile(t.Context(), reqFor("3buqdlot")); err == nil {
		t.Fatal("a read error is returned, never a pass")
	}
	if tk = g.task(t); tk.Status.Phase != v1alpha1.PhaseAwaitingCI {
		t.Fatalf("%s", tk.Status.Phase)
	}
}
