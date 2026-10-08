// SPDX-License-Identifier: Apache-2.0

package envelope

import (
	"encoding/json"
	"testing"
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
	for name, f := range map[string]TaskFacts{
		"no phase":       {},
		"negative usage": {Phase: "Implementing", Budget: &BudgetFact{UsedTokens: -1}},
		"non-github pr":  {Phase: "AwaitingHuman", PR: &PRFact{Number: 1, URL: "https://evil.example/pull/1"}},
	} {
		if f.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := (TaskFacts{Phase: "Queued"}).Validate(); err != nil {
		t.Fatalf("minimal facts refused: %v", err)
	}
}
