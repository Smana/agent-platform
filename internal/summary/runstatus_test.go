// SPDX-License-Identifier: Apache-2.0

package summary

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/Smana/maku/internal/envelope"
	"github.com/Smana/maku/internal/policy"
)

type statusCase struct {
	Name string `json:"name"`
	In   struct {
		State, Phase string
		ReasonCode   string `json:"reasonCode"`
		RunRole      string `json:"runRole"`
		RunStartedAt string `json:"runStartedAt"`
		RunPresent   bool   `json:"runPresent"`
		PR, Issue    int
		Mine, Others int
		Step         *Step
		Now          string
	} `json:"in"`
	Want RunStatus `json:"want"`
}

// The cases are shared with the page (web/test/run-status.test.ts): both sides say the same words.
func TestStatusLineCases(t *testing.T) {
	raw, err := os.ReadFile("runstatus_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []statusCase
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			in := StatusIn{State: c.In.State, Phase: c.In.Phase, ReasonCode: c.In.ReasonCode, RunRole: c.In.RunRole,
				RunPresent: c.In.RunPresent, PR: c.In.PR, Issue: c.In.Issue, Mine: c.In.Mine, Others: c.In.Others, Step: c.In.Step}
			in.RunStart, _ = time.Parse(time.RFC3339, c.In.RunStartedAt)
			in.Now, _ = time.Parse(time.RFC3339, c.In.Now)
			if got := StatusLine(in); got != c.Want {
				t.Errorf("got %#v, want %#v", got, c.Want)
			}
		})
	}
}

func TestStatusOfReadsTheSummary(t *testing.T) {
	var s Summary
	s.Status.Phase = "Implementing"
	run := envelope.RunFact{ID: "r1", Role: "implementer", StartedAt: time.Date(2026, 10, 28, 10, 0, 0, 0, time.UTC)}
	s.Status.Run = &run
	s.NeedsYou = []Need{{Kind: "approval"}}
	if got := StatusOf(s, time.Now()); got.Kind != KindWaitingYou {
		t.Errorf("an approval for the viewer leads: %+v", got)
	}
	s.NeedsYou = nil
	if got := StatusOf(s, run.StartedAt.Add(4*time.Minute)); got.Words != "Running — implementer, 4 min" {
		t.Errorf("the coarse state is read from the phase: %+v", got)
	}
}

// The summary carries the factory's raw reason code beside its prose, so the status line reads the
// code on every surface: the page, maku status and the room list.
func TestViewCarriesTheReasonCode(t *testing.T) {
	st := State{Facts: &envelope.TaskFacts{Phase: "Rejected", Reason: "ci_red"}, Pending: map[string]Approval{}}
	v := View(st, "r", "u", policy.Subject{}, 0, time.Now())
	if v.Status.ReasonCode != "ci_red" {
		t.Fatalf("reasonCode = %q", v.Status.ReasonCode)
	}
	v.Status.State = StateFailed
	if got := StatusOf(v, time.Now()); got.Words != "Failed — CI stayed red" {
		t.Fatalf("StatusOf reads the code: %+v", got)
	}
}
