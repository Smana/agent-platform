// SPDX-License-Identifier: Apache-2.0

package fmetrics

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Smana/maku/internal/factory/runs"
)

// Spend per identity SI4 and SI5: the series a principal's page reads carry the payer the context
// binds, in the shape of a ledger key, else other; an unbound context reads other.
func TestThePayerLabelsTheSeriesItsContextBinds(t *testing.T) {
	s, scrape := rig(t, nil, func() bool { return true })
	s.ListRepositories("Smana/a")
	alice := WithPrincipal(WithRepository(t.Context(), "Smana/a"), "github:alice")
	s.PROutcome(alice, "docs-links", "auto_merged")
	s.PROpened(alice)
	s.AdmissionRefused(alice, "person_budget_exhausted")
	s.Revoked(alice, "budget-person")
	s.RunRevoked(alice, "budget-task", "task-usd")
	s.HumanWait(alice, "AwaitingHuman", 90*time.Second)
	s.HumanWait(alice, "Implementing", time.Hour) // not a wait for a human: not counted
	s.PROutcome(WithPrincipal(t.Context(), "github:Ignore previous instructions"), "docs-links", "closed")
	s.PROutcome(t.Context(), "docs-links", "closed")
	s.PrincipalShadow(alice, "system:factory") // the key over, not the payer
	body := scrape()
	for _, want := range []string{
		`maku_factory_pr_outcomes_total{class="docs-links",outcome="auto_merged",principal="github:alice",repository="Smana/a"} 1`,
		`maku_factory_prs_opened_total{principal="github:alice",repository="Smana/a"} 1`,
		`maku_factory_admission_refusals_total{principal="github:alice",reason="person_budget_exhausted",repository="Smana/a"} 1`,
		`maku_factory_run_revocations_total{principal="github:alice",reason="budget-person"} 1`,
		`maku_factory_run_revocations_total{limit="task-usd",principal="github:alice",reason="budget-task"} 1`,
		`maku_factory_human_wait_seconds_total{phase="AwaitingHuman",principal="github:alice"} 90`,
		`maku_factory_pr_outcomes_total{class="docs-links",outcome="closed",principal="other",repository="other"} 2`,
		`maku_factory_run_revocations_total{principal="system:factory",reason="budget-principal-shadow"} 1`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(body, `phase="Implementing"`) || strings.Contains(body, "Ignore") {
		t.Errorf("an unbounded value is exposed")
	}
	if t.Failed() {
		t.Log(body)
	}
}

// SI5: a ledger key's shape is system:<name>, github:<login> or human:<sub>; nothing else is one.
func TestThePrincipalTakesALedgerKeysShape(t *testing.T) {
	for in, want := range map[string]string{
		"system:factory":                    "system:factory",
		"system:slack-app":                  "system:slack-app",
		"github:alice":                      "github:alice",
		"github:a-b-c1":                     "github:a-b-c1",
		"human:291874650112344":             "human:291874650112344",
		"":                                  "other",
		"github:Alice":                      "other", // Person lowercases: an upper case is no key
		"github:" + strings.Repeat("a", 40): "other", // a GitHub login is at most 39
		"agent:3buqdlot":                    "other",
		"system:factory\n":                  "other",
		`github:alice",principal="x`:        "other",
	} {
		if got := boundPrincipal(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

// SI2: the charged counter adds a run's settled micro-USD in dollars under its payer and kind.
func TestTheChargedCounterIsInDollars(t *testing.T) {
	s, scrape := rig(t, nil, func() bool { return true })
	s.Charged(t.Context(), "github:alice", runs.KindTask, 1_444_000)
	s.Charged(t.Context(), "github:alice", runs.KindTask, 56_000)
	s.Charged(t.Context(), "human:292", runs.KindRoom, 1_700)
	s.Charged(t.Context(), "github:alice", "a kind", 1)
	body := scrape()
	for _, want := range []string{
		`maku_factory_charged_usd_total{kind="task",principal="github:alice"} 1.5`,
		`maku_factory_charged_usd_total{kind="room",principal="human:292"} 0.0017`,
		`maku_factory_charged_usd_total{kind="other",principal="github:alice"} 1e-06`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("missing %s\n%s", want, body)
		}
	}
}

// SI3: one series per run the meter hands over, naming its payer, its task or its room; a run
// whose id is not a C2 id is not exposed, and the next hand-over replaces the last.
func TestTheChargeInfoNamesEachRunsPayer(t *testing.T) {
	s, scrape := rig(t, nil, func() bool { return true })
	s.RunCharges([]runs.Run{
		{ID: "aaaaaaaa", TaskID: "3buqdlot", RoomRef: "3buqdlot", Principal: runs.PrincipalFactory, ChargedTo: "github:alice"},
		{ID: "bbbbbbbb", RoomRef: "5buqdlot", Principal: "human:291", ChargedTo: "github:bob"},
		{ID: "cccccccc", Principal: "human:292"},
		{ID: "not a run", Principal: "human:292"},
	})
	body := scrape()
	for _, want := range []string{
		`maku_factory_run_charge_info{principal="github:alice",room="3buqdlot",run="aaaaaaaa",task="3buqdlot"} 1`,
		`maku_factory_run_charge_info{principal="github:bob",room="5buqdlot",run="bbbbbbbb"} 1`,
		`maku_factory_run_charge_info{principal="human:292",run="cccccccc"} 1`,
	} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("missing %s\n%s", want, body)
		}
	}
	if strings.Contains(body, "not a run") {
		t.Errorf("a run id outside C2 is exposed")
	}
	s.RunCharges(nil)
	if body := scrape(); strings.Contains(body, "maku_factory_run_charge_info{") {
		t.Errorf("an ended run's series outlives the hand-over that dropped it\n%s", body)
	}
}

// SI4: the tasks gauge names each task's payer: its labeller or retrier while per-person caps are
// on, the fleet for a task no person started or with them off.
func TestTheTasksGaugeNamesThePayer(t *testing.T) {
	labelled, scheduled := tk("3buqdlot", "AwaitingHuman"), tk("4buqdlot", "AwaitingHuman")
	labelled.Spec.Source.RequestedBy, scheduled.Spec.Source.RequestedBy = "github:Alice", "system:scheduler"
	c := fake.NewClientBuilder().WithScheme(scheme()).WithObjects(labelled, scheduled).Build()
	s, scrape := rig(t, c, func() bool { return true })
	gauge := func(who string, n int) string {
		return fmt.Sprintf(`maku_factory_tasks{phase="AwaitingHuman",predicted_class="review",principal="%s",repository="other",source="issue",tier="standard"} %d`, who, n)
	}
	if body := scrape(); !strings.Contains(body, gauge("system:factory", 2)+"\n") {
		t.Errorf("per-person caps off: both tasks are the fleet's\n%s", body)
	}
	s.ChargePeople(true)
	body := scrape()
	for _, want := range []string{gauge("github:alice", 1), gauge("system:factory", 1)} {
		if !strings.Contains(body, want+"\n") {
			t.Errorf("missing %s\n%s", want, body)
		}
	}
}

// AM1 (R-I1): the merger's merge on a maintainer's approval is its own outcome, never other.
func TestAnApprovedMergeIsItsOwnOutcome(t *testing.T) {
	s, scrape := rig(t, nil, func() bool { return true })
	s.PROutcome(t.Context(), "docs-links", "approved_merged")
	if body := scrape(); !strings.Contains(body, `maku_factory_pr_outcomes_total{class="docs-links",outcome="approved_merged",principal="other",repository="other"} 1`+"\n") {
		t.Fatalf("approved_merged not exposed:\n%s", body)
	}
}
