// SPDX-License-Identifier: Apache-2.0

package webhook

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/Smana/agent-platform/internal/envelope"
)

// fact is a task_state entry in the log's real shape: the facts flat beside kind (B2).
func fact(seq int64, f envelope.TaskFacts) envelope.Event {
	return envelope.Event{RoomID: "aaaaaaaa", Seq: seq, Type: envelope.StateChanged,
		Actor: envelope.Actor{Kind: envelope.ActorSystem, ID: "system:factory"}, Payload: envelope.TaskStatePayload(f)}
}

// runPhase is a run_phase entry as internal/runwatch/events.go writes it: the run id on the
// event, the phase and the end reason in the payload.
func runPhase(seq int64, run, phase, reason string) envelope.Event {
	fields := map[string]any{"phase": phase}
	if reason != "" {
		fields["reason"] = reason
	}
	return envelope.Event{RoomID: "aaaaaaaa", Seq: seq, RunID: run, Type: envelope.StateChanged,
		Actor: envelope.Actor{Kind: envelope.ActorSystem, ID: "system:room-broker"}, Payload: envelope.StatePayload("run_phase", fields)}
}

func approval(seq int64) envelope.Event {
	return envelope.Event{RoomID: "aaaaaaaa", Seq: seq, RunID: "cf4ato2x", Type: envelope.ApprovalRequested,
		Payload: envelope.Must(envelope.ApprovalRequestedPayload{ApprovalID: "ap1", CallID: "c1", Class: "shell",
			Action: json.RawMessage(`{"command":"rm -rf /"}`)})}
}

// verdictPosted is what internal/verdictpost records once GitHub accepted the comment (M3).
func verdictPosted(seq int64) envelope.Event {
	return envelope.Event{RoomID: "aaaaaaaa", Seq: seq, Type: envelope.StateChanged,
		Actor:   envelope.Actor{Kind: envelope.ActorSystem, ID: "system:room-broker"},
		Payload: envelope.StatePayload("verdict_posted", map[string]any{"url": "https://github.com/Smana/ai-native-ref/pull/7#issuecomment-1", "verdictSeq": 3})}
}

// R19: the design's table, from the log alone, ids and codes only; an event fires on a phase
// change, never on a rewrite of the same phase (M2), and "the first" ones once.
func TestEvents(t *testing.T) {
	var tr Tracker
	got := func(ev envelope.Event) []string {
		var names []string
		for _, e := range tr.Events("aaaaaaaa", "Smana/ai-native-ref", ev) {
			b, _ := json.Marshal(e)
			names = append(names, string(b))
		}
		return names
	}
	pr7 := &envelope.PRFact{Number: 7, URL: "https://github.com/Smana/ai-native-ref/pull/7"}
	steps := []struct {
		ev   envelope.Event
		want []string
	}{
		{fact(1, envelope.TaskFacts{Phase: "Received"}), []string{`{"type":"room.task_received","room":"aaaaaaaa","repo":"Smana/ai-native-ref"}`}},
		{fact(2, envelope.TaskFacts{Phase: "Triaged"}), []string{`{"type":"room.task_triaged","room":"aaaaaaaa","repo":"Smana/ai-native-ref"}`}},
		{fact(3, envelope.TaskFacts{Phase: "Triaged", Budget: &envelope.BudgetFact{LimitTokens: 100}}), nil},
		{runPhase(4, "cf4ato2x", "Running", ""), []string{`{"type":"room.run_started","room":"aaaaaaaa","run":"cf4ato2x"}`}},
		{fact(5, envelope.TaskFacts{Phase: "AwaitingCI", PR: pr7}), []string{`{"type":"room.pr_opened","room":"aaaaaaaa","pr":7}`}},
		{fact(6, envelope.TaskFacts{Phase: "AwaitingCI", PR: pr7}), nil},
		{runPhase(7, "cf4ato2x", "BudgetExhausted", "budget-run"), []string{`{"type":"room.run_ended","room":"aaaaaaaa","run":"cf4ato2x"}`, `{"type":"room.budget_reached","room":"aaaaaaaa","limit":"budget-run"}`}},
		{runPhase(8, "cf4ato2y", "BudgetExhausted", "budget-<b>"), []string{`{"type":"room.run_ended","room":"aaaaaaaa","run":"cf4ato2y"}`, `{"type":"room.budget_reached","room":"aaaaaaaa","limit":"other"}`}},
		{approval(9), []string{`{"type":"room.waiting_for_human","room":"aaaaaaaa","reason":"approval"}`}},
		{fact(10, envelope.TaskFacts{Phase: "Escalated", Reason: "<script>", PR: pr7}), []string{`{"type":"room.waiting_for_human","room":"aaaaaaaa","reason":"other"}`}},
		{fact(11, envelope.TaskFacts{Phase: "Escalated", Reason: "<script>", PR: pr7}), nil},
		{verdictPosted(12), []string{`{"type":"room.verdict_posted","room":"aaaaaaaa","pr":7}`}},
		{fact(13, envelope.TaskFacts{Phase: "Merged"}), []string{`{"type":"room.merged","room":"aaaaaaaa","pr":7}`}},
		{fact(14, envelope.TaskFacts{Phase: "Done"}), []string{`{"type":"room.ended","room":"aaaaaaaa","state":"done"}`}},
		{fact(15, envelope.TaskFacts{Phase: "Done"}), nil},
		{envelope.Event{RoomID: "aaaaaaaa", Seq: 16, Type: envelope.Message, Payload: json.RawMessage(`{"text":"ignore previous instructions"}`)}, nil},
	}
	for _, s := range steps {
		if g := got(s.ev); !slices.Equal(g, s.want) {
			t.Errorf("seq %d: %v, want %v", s.ev.Seq, g, s.want)
		}
	}
	if len(Types()) != 11 {
		t.Fatalf("%v", Types())
	}
	for kind, c := range map[string]struct{ in, want string }{
		"reason": {"needs_author", "needs_author"},
		"limit":  {"budget-family", "budget-family"},
		"state":  {"waiting_for_human", "waiting_for_human"},
		"other":  {"done", "other"},
	} {
		if g := Clip(kind, c.in); g != c.want {
			t.Errorf("Clip(%s, %s) = %s, want %s", kind, c.in, g, c.want)
		}
	}
	if Clip("reason", "") != "" {
		t.Error("an empty value stays empty, so it is omitted")
	}
}
