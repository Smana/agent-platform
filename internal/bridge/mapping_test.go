// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Smana/agent-platform/internal/envelope"
)

const runID = "7f3cq2xz"

func raw(t *testing.T, ev map[string]any) RawEvent {
	t.Helper()
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	var r RawEvent
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func text(s string) []any { return []any{map[string]any{"type": "text", "text": s}} }

// mappingCases are the spec table (§3 Event mapping) in agent-server 1.49.6's
// event shapes. has is looked for in the last item's payload.
var mappingCases = []struct {
	name  string
	ev    map[string]any
	types []envelope.Type
	has   string
}{
	{"an agent message is a chat", map[string]any{"id": "1", "kind": "MessageEvent", "source": "agent",
		"llm_message": map[string]any{"role": "assistant", "content": text("Done.")}},
		[]envelope.Type{envelope.Message}, `"text":"Done."`},
	{"a user message is addressed to the agent", map[string]any{"id": "2", "kind": "MessageEvent", "source": "user",
		"llm_message": map[string]any{"role": "user", "content": text("Fix it")}},
		[]envelope.Type{envelope.Message}, `"to":["agent:7f3cq2xz"]`},
	{"an action with a thought is a message then a tool call", map[string]any{"id": "3", "kind": "ActionEvent",
		"source": "agent", "tool_name": "terminal", "tool_call_id": "c1", "security_risk": "HIGH",
		"action": map[string]any{"command": "ls"}, "thought": text("list files")},
		[]envelope.Type{envelope.Message, envelope.ToolCall}, `"callId":"c1"`},
	{"an action without a thought is a tool call with empty args", map[string]any{"id": "3b", "kind": "ActionEvent",
		"source": "agent", "tool_name": "terminal", "tool_call_id": "c1"},
		[]envelope.Type{envelope.ToolCall}, `"args":{}`},
	{"an observation is an ok result", map[string]any{"id": "4", "kind": "ObservationEvent", "source": "environment",
		"tool_name": "terminal", "tool_call_id": "c1", "observation": map[string]any{"content": text("a b")}},
		[]envelope.Type{envelope.ToolResult}, `"status":"ok"`},
	{"an observation marked is_error is an error result", map[string]any{"id": "4b", "kind": "ObservationEvent",
		"tool_call_id": "c1", "observation": map[string]any{"content": text("no such file"), "is_error": true}},
		[]envelope.Type{envelope.ToolResult}, `"status":"error"`},
	{"a user rejection is a rejected result", map[string]any{"id": "5", "kind": "UserRejectObservation",
		"source": "user", "tool_name": "terminal", "tool_call_id": "c2", "rejection_reason": "denied"},
		[]envelope.Type{envelope.ToolResult}, `"status":"rejected"`},
	{"an agent error is an error result", map[string]any{"id": "6", "kind": "AgentErrorEvent", "source": "agent",
		"tool_name": "terminal", "tool_call_id": "c3", "error": "boom"},
		[]envelope.Type{envelope.ToolResult}, `"status":"error"`},
	{"a conversation error is a harness_error", map[string]any{"id": "7", "kind": "ConversationErrorEvent",
		"source": "environment", "code": "LLMError", "detail": "429"},
		[]envelope.Type{envelope.StateChanged}, `"kind":"harness_error"`},
	{"a pause is a harness_paused", map[string]any{"id": "7b", "kind": "PauseEvent", "source": "user"},
		[]envelope.Type{envelope.StateChanged}, `"kind":"harness_paused"`},
	{"an interrupt is recorded, and the status tracker cancels the turn", map[string]any{"id": "8",
		"kind": "InterruptEvent", "source": "user"},
		[]envelope.Type{envelope.StateChanged}, `"kind":"interrupted"`},
	{"the system prompt is dropped", map[string]any{"id": "9", "kind": "SystemPromptEvent", "source": "agent"}, nil, ""},
	{"a streaming delta is dropped", map[string]any{"id": "10", "kind": "StreamingDeltaEvent", "source": "agent"}, nil, ""},
	{"a state update is dropped (status is polled)", map[string]any{"id": "11", "kind": "ConversationStateUpdateEvent",
		"source": "environment"}, nil, ""},
	{"an unknown kind records that it happened, not what it said", map[string]any{"id": "12", "kind": "SomethingNew",
		"source": "agent", "secret": "hunter2"},
		[]envelope.Type{envelope.StateChanged}, `"harnessKind":"SomethingNew"`},
	{"a known kind with fields of the wrong type still maps", map[string]any{"id": "13", "kind": "ActionEvent",
		"tool_call_id": 42, "thought": "not a list", "action": "ls"},
		[]envelope.Type{envelope.ToolCall}, `"args":"ls"`},
	{"an event with no id records that a malformed event happened", map[string]any{"kind": "MessageEvent",
		"llm_message": map[string]any{"content": text("hunter2")}},
		[]envelope.Type{envelope.StateChanged}, `"harnessKind":"malformed"`},
	{"an event with a non-string id records that a malformed event happened", map[string]any{"id": 42,
		"kind": "ObservationEvent", "tool_call_id": "hunter2"},
		[]envelope.Type{envelope.StateChanged}, `"harnessKind":"malformed"`},
}

func TestMapFollowsTheSpecTable(t *testing.T) {
	for _, c := range mappingCases {
		t.Run(c.name, func(t *testing.T) {
			got := Map(raw(t, c.ev), runID)
			if len(got) != len(c.types) {
				t.Fatalf("%d items, want %d", len(got), len(c.types))
			}
			for i, m := range got {
				if m.Type != c.types[i] {
					t.Errorf("item %d is %s, want %s", i, m.Type, c.types[i])
				}
			}
			if c.has != "" && !strings.Contains(string(got[len(got)-1].Payload), c.has) {
				t.Errorf("%s lacks %s", got[len(got)-1].Payload, c.has)
			}
			for _, m := range got {
				if strings.Contains(string(m.Payload), "hunter2") {
					t.Error("an unknown event's content reached the log")
				}
			}
		})
	}
}

// Every item the mapping produces is one the log can hold: a valid draft, a JSON
// object, within MaxPayload and within the event's seq slots.
func TestMappedItemsAreValidDrafts(t *testing.T) {
	huge := strings.Repeat("y", 3<<20)
	events := []map[string]any{
		{"id": "h1", "kind": "MessageEvent", "source": "agent", "llm_message": map[string]any{"content": text(huge)}},
		{"id": "h2", "kind": "ActionEvent", "tool_call_id": "c", "action": map[string]any{"file_text": huge}, "thought": text(huge)},
		{"id": "h3", "kind": "ObservationEvent", "tool_call_id": "c",
			"observation": map[string]any{"content": text(strings.Repeat("\x01", envelope.MaxToolOutput))}},
		{"id": "h4", "kind": "ConversationErrorEvent", "code": huge, "detail": huge},
		{"id": "h5", "kind": huge},
		{"id": "h6", "kind": "MessageEvent", "llm_message": map[string]any{"content": text(strings.Repeat("\x02é", 1<<16))}},
	}
	for _, c := range mappingCases {
		events = append(events, c.ev)
	}
	for _, ev := range events {
		items := Map(raw(t, ev), runID)
		if len(items) > ItemsPerEvent {
			t.Fatalf("%s: %d items overflow the event's seq slots", ev["id"], len(items))
		}
		for k, m := range items {
			assertStorable(t, m, SeqFor(1, k))
		}
	}
}

func assertStorable(t *testing.T, m Mapped, seq int64) {
	t.Helper()
	d := envelope.Draft{RoomID: "3kq7x2ma", RunID: runID, Actor: envelope.Actor{Kind: envelope.ActorAgent, ID: "run:" + runID},
		Type: m.Type, Origin: envelope.OriginHarness, OriginClient: "agent:" + runID, OriginSeq: seq, Payload: m.Payload}
	if err := d.Validate(); err != nil {
		t.Errorf("%s: %v", m.Type, err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(m.Payload, &obj); err != nil || obj == nil {
		t.Errorf("%s: the payload is not a JSON object", m.Type)
	}
	if len(m.Payload) > envelope.MaxPayload {
		t.Errorf("%s: %d bytes over MaxPayload", m.Type, len(m.Payload))
	}
}

// tightFit is how far under MaxPayload a cut payload may end: one more rune, at
// most a six-byte escape, would not have fit.
const tightFit = 6

func TestOversizeItemsFitTheLog(t *testing.T) {
	huge := strings.Repeat("y", 3<<20)
	cases := []struct {
		name   string
		ev     map[string]any
		check  func(Mapped) bool
		reason string
	}{
		{"a tool call over MaxPayload is the oversize stub",
			map[string]any{"id": "1", "kind": "ActionEvent", "tool_call_id": "c", "action": map[string]any{"file_text": huge}},
			func(m Mapped) bool {
				var p struct {
					Oversize bool
					Bytes    int
					Type     envelope.Type
				}
				return json.Unmarshal(m.Payload, &p) == nil && p.Oversize && p.Type == envelope.ToolCall && p.Bytes > len(huge)
			},
			"the stub records the type and the real size"},
		{"a tool result whose escaped output passes MaxPayload keeps its call, status and the longest prefix that fits",
			map[string]any{"id": "2", "kind": "ObservationEvent", "tool_call_id": "c",
				"observation": map[string]any{"content": text(strings.Repeat("\x01", envelope.MaxToolOutput))}},
			func(m Mapped) bool {
				var p envelope.ToolResultPayload
				return json.Unmarshal(m.Payload, &p) == nil && p.CallID == "c" && p.Status == "ok" && p.Truncated &&
					p.Bytes == envelope.MaxToolOutput && len(p.Output) > 0 && strings.Trim(p.Output, "\x01") == "" &&
					len(m.Payload) > envelope.MaxPayload-tightFit
			},
			"16 KiB of control bytes escape to 96 KiB, and the pairing with its tool_call keys on callId"},
		{"a message over MaxPayload keeps its start and says it was cut",
			map[string]any{"id": "3", "kind": "MessageEvent", "llm_message": map[string]any{"content": text(huge)}},
			func(m Mapped) bool {
				var p envelope.MessagePayload
				return json.Unmarshal(m.Payload, &p) == nil && strings.HasPrefix(p.Text, "yyyy") &&
					strings.HasSuffix(p.Text, truncatedMark) && len(m.Payload) > envelope.MaxPayload-tightFit
			},
			"the broker refuses an oversize stub for a message, so its text is cut instead"},
		{"an escape-heavy message keeps the longest prefix that fits, not nothing",
			map[string]any{"id": "3b", "kind": "MessageEvent", "llm_message": map[string]any{"content": text(strings.Repeat("\x02é", 1<<16))}},
			func(m Mapped) bool {
				var p envelope.MessagePayload
				return json.Unmarshal(m.Payload, &p) == nil && strings.HasPrefix(p.Text, "\x02é") &&
					strings.HasSuffix(p.Text, truncatedMark) && len(m.Payload) > envelope.MaxPayload-tightFit
			},
			"each \\x02 escapes to six bytes, so a raw-byte estimate of the cut keeps nothing"},
		{"a harness error keeps its kind and a bounded detail",
			map[string]any{"id": "4", "kind": "ConversationErrorEvent", "code": "LLMError", "detail": huge},
			func(m Mapped) bool {
				var p map[string]string
				return json.Unmarshal(m.Payload, &p) == nil && p["kind"] == "harness_error" && p["code"] == "LLMError" &&
					len(p["detail"]) == maxStateField
			},
			"the broker refuses a state_changed without its kind"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Map(raw(t, c.ev), runID)
			if len(got) != 1 || !c.check(got[0]) {
				t.Fatalf("%s: got %.200s", c.reason, got)
			}
		})
	}
}

func TestToolOutputIsTruncated(t *testing.T) {
	cases := []struct {
		name string
		out  string
		keep int
	}{
		{"ascii is cut at MaxToolOutput", strings.Repeat("x", envelope.MaxToolOutput+100), envelope.MaxToolOutput},
		{"a rune is never split", strings.Repeat("x", envelope.MaxToolOutput-1) + "é" + "tail", envelope.MaxToolOutput - 1},
		{"what fits is kept whole", "short", len("short")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Map(raw(t, map[string]any{"id": "1", "kind": "ObservationEvent", "tool_call_id": "c",
				"observation": map[string]any{"content": text(c.out)}}), runID)
			var p envelope.ToolResultPayload
			if err := json.Unmarshal(got[0].Payload, &p); err != nil {
				t.Fatal(err)
			}
			cut := len(c.out) > envelope.MaxToolOutput
			if p.Truncated != cut || len(p.Output) != c.keep || p.Bytes != len(c.out) || !utf8.ValidString(p.Output) {
				t.Fatalf("truncated=%v len=%d bytes=%d", p.Truncated, len(p.Output), p.Bytes)
			}
		})
	}
}

func TestStatusTransitionsBecomeTurnsAndStateChanges(t *testing.T) {
	var st StatusTracker
	var all []StatusItem
	// running → waiting_for_confirmation → running stays one turn; finished ends it.
	for _, s := range []string{"idle", "running", "waiting_for_confirmation", "running", "finished", "running", "error"} {
		all = append(all, st.Observe(s, runID)...)
	}
	var b strings.Builder
	for k, m := range all {
		if m.Seq != int64(k+1) {
			t.Fatalf("item %d has status seq %d: the stream must be gapless", k, m.Seq)
		}
		assertStorable(t, m.Mapped, m.Seq)
		b.WriteString(string(m.Type) + ":" + string(m.Payload) + "\n")
	}
	out := b.String()
	for _, want := range []string{`"status":"running"`, `"phase":"started"`, `"status":"error"`, `"phase":"failed"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in\n%s", want, out)
		}
	}
	if strings.Count(out, `"phase":"started"`) != 2 || strings.Count(out, `"phase":"completed"`) != 1 {
		t.Errorf("want two turns, the first completed and the second failed:\n%s", out)
	}
	if again := st.Observe("error", runID); again != nil {
		t.Errorf("an unchanged status produced %d items", len(again))
	}
}

// Ruling AL (b): turn ids derive from the status stream's seq, so a bridge
// resumed after the log's status cursor never names a turn as an earlier one did.
func TestResumedTrackersNeverReuseATurnID(t *testing.T) {
	turnIDs := func(items []StatusItem) []string {
		var out []string
		for _, it := range items {
			var p envelope.TurnPayload
			if it.Type == envelope.Turn && json.Unmarshal(it.Payload, &p) == nil {
				out = append(out, p.TurnID)
			}
		}
		return out
	}
	cases := []struct {
		name     string
		statuses []string
		want     []string
	}{
		{"a turn started and ended keeps one id", []string{"running", "finished"}, []string{"t12", "t12"}},
		{"a turn ended without its start still gets an unused id", []string{"waiting_for_confirmation", "running", "finished"},
			[]string{"t14"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var first StatusTracker
			for _, s := range []string{"running", "finished"} {
				first.Observe(s, runID)
			}
			// The first bridge used seqs 1–4 and named its turn t2; the log holds 10.
			var st StatusTracker
			st.Resume(10)
			var items []StatusItem
			for _, s := range c.statuses {
				items = append(items, st.Observe(s, runID)...)
			}
			if items[0].Seq != 11 {
				t.Fatalf("resumed at seq %d, want 11", items[0].Seq)
			}
			if got := turnIDs(items); !slices.Equal(got, c.want) {
				t.Fatalf("turn ids %v, want %v", got, c.want)
			}
		})
	}
}

func TestSeqFor(t *testing.T) {
	cases := []struct {
		event int64
		k     int
		want  int64
	}{{1, 0, 4}, {1, 3, 7}, {2, 0, 8}}
	for _, c := range cases {
		if got := SeqFor(c.event, c.k); got != c.want {
			t.Errorf("SeqFor(%d, %d) = %d, want %d: event i owns 4i..4i+3", c.event, c.k, got, c.want)
		}
	}
}
