// SPDX-License-Identifier: Apache-2.0

package envelope

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestTaskStatePayloadCarriesKindAndFacts(t *testing.T) {
	f := TaskFacts{Phase: "Implementing", Run: &RunFact{ID: "cf4ato2x", Role: "implementer", Trigger: "human"},
		Budget: &BudgetFact{UsedTokens: 189093, LimitTokens: 1500000},
		Issue:  &IssueFact{Number: 2238, URL: "https://github.com/Smana/cloud-native-ref/issues/2238", Author: "smana", LabelledBy: "smana"},
		PR:     &PRFact{Number: 2239, URL: "https://github.com/Smana/cloud-native-ref/pull/2239", Author: "ogenki-agent-factory[bot]", Reviewers: []string{"smana"}}}
	var got map[string]any
	if err := json.Unmarshal(TaskStatePayload(f), &got); err != nil {
		t.Fatal(err)
	}
	if got["kind"] != "task" || got["phase"] != "Implementing" || got["pr"].(map[string]any)["number"] != float64(2239) {
		t.Fatalf("payload %v", got)
	}
}

func TestTaskFactsValidate(t *testing.T) {
	t0 := time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC)
	runs := func(n int) []RunFact {
		r := make([]RunFact, n)
		for i := range r {
			r[i] = RunFact{ID: "r", Role: "implementer"}
		}
		return r
	}
	bad2 := runs(3)
	bad2[2].MicroUSD = -1
	for name, tc := range map[string]struct {
		f    TaskFacts
		want string // "" means any error
	}{
		"no phase":        {TaskFacts{}, ""},
		"negative usage":  {TaskFacts{Phase: "Implementing", Budget: &BudgetFact{UsedTokens: -1}}, ""},
		"non-github pr":   {TaskFacts{Phase: "AwaitingHuman", PR: &PRFact{Number: 1, URL: "https://evil.example/pull/1"}}, ""},
		"negative used":   {TaskFacts{Phase: "Queued", Spend: &SpendFact{UsedMicroUSD: -1}}, "usedMicroUSD"},
		"negative limit":  {TaskFacts{Phase: "Queued", Spend: &SpendFact{LimitMicroUSD: -1}}, "limitMicroUSD"},
		"negative run":    {TaskFacts{Phase: "Queued", Run: &RunFact{ID: "r", MicroUSD: -1}}, "microUSD"},
		"negative runs 2": {TaskFacts{Phase: "Queued", Runs: bad2}, "runs[2]"},
		"21 runs":         {TaskFacts{Phase: "Queued", Runs: runs(21)}, "runs"},
		"stalled":         {TaskFacts{Phase: "Queued", Runs: []RunFact{{ID: "r", Outcome: "stalled"}}}, "outcome"},
		"long verdict":    {TaskFacts{Phase: "Queued", Runs: []RunFact{{ID: "r", Verdict: strings.Repeat("v", 33)}}}, "verdict"},
		"ended first":     {TaskFacts{Phase: "Queued", Runs: []RunFact{{ID: "r", StartedAt: t0, EndedAt: t0.Add(-time.Second)}}}, "endedAt"},
	} {
		err := tc.f.Validate()
		if err == nil {
			t.Errorf("%s: accepted", name)
		} else if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q does not name %q", name, err, tc.want)
		}
	}
	if err := (TaskFacts{Phase: "Queued", Runs: runs(MaxRuns)}).Validate(); err != nil {
		t.Fatalf("%d runs refused: %v", MaxRuns, err)
	}
	if err := (TaskFacts{Phase: "Queued", Runs: []RunFact{{ID: "r", Outcome: OutcomeSucceeded, Verdict: strings.Repeat("v", 32), StartedAt: t0, EndedAt: t0}}}).Validate(); err != nil {
		t.Fatalf("valid run refused: %v", err)
	}
	if err := (TaskFacts{Phase: "Queued"}).Validate(); err != nil {
		t.Fatalf("minimal facts refused: %v", err)
	}
}

func TestTaskFactsRoundTripsRunsAndDollars(t *testing.T) {
	t0 := time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC)
	want := TaskFacts{Phase: "Done",
		Runs: []RunFact{
			{ID: "a", Role: "implementer", StartedAt: t0, EndedAt: t0.Add(time.Hour), Outcome: OutcomeSucceeded, MicroUSD: 1_250_000},
			{ID: "b", Role: "reviewer", StartedAt: t0.Add(time.Hour), EndedAt: t0.Add(2 * time.Hour), Outcome: OutcomeFailed, Verdict: "approve", MicroUSD: 40_000},
		},
		Budget: &BudgetFact{UsedTokens: 10, LimitTokens: 20},
		Spend:  &SpendFact{UsedMicroUSD: 1_290_000, LimitMicroUSD: 1_500_000}}
	b, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var got TaskFacts
	if err := dec.Decode(&got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip\n got %+v\nwant %+v", got, want)
	}
}

func TestGitHubURL(t *testing.T) {
	if !GitHubURL("https://github.com/Smana/a/pull/7") {
		t.Error("a github.com pull request refused")
	}
	if GitHubURL("https://github.com.evil/Smana/a/pull/7") {
		t.Error("a lookalike host accepted")
	}
}
