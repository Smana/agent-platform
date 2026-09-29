// SPDX-License-Identifier: Apache-2.0

package bridgeapi

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Smana/agent-platform/internal/bridge"
	"github.com/Smana/agent-platform/internal/envelope"
)

// An honest harness never trips the broker's accept rules: every item the
// bridge's own mapping produces is accepted and stored byte for byte as sent,
// oversize and malformed harness events included.
func TestTheBridgeMappingPassesTheAcceptRules(t *testing.T) {
	huge := strings.Repeat("y", 3<<20)
	text := func(s string) []any { return []any{map[string]any{"type": "text", "text": s}} }
	events := []map[string]any{
		{"kind": "MessageEvent", "source": "agent", "llm_message": map[string]any{"content": text("Done.")}},
		{"kind": "MessageEvent", "source": "user", "llm_message": map[string]any{"content": text("Fix it")}},
		{"kind": "MessageEvent", "source": "agent", "llm_message": map[string]any{"content": text(huge)}},
		{"kind": "ActionEvent", "tool_name": "terminal", "tool_call_id": "c1", "security_risk": "HIGH",
			"action": map[string]any{"command": "ls", "Command": "rm"}, "thought": text("list files")},
		{"kind": "ActionEvent", "tool_call_id": "c1", "action": map[string]any{"file_text": huge}, "thought": text(huge)},
		{"kind": "ObservationEvent", "tool_call_id": "c1", "observation": map[string]any{"content": text("a b"), "is_error": true}},
		{"kind": "ObservationEvent", "tool_call_id": "c1",
			"observation": map[string]any{"content": text(strings.Repeat("\x01", envelope.MaxToolOutput))}},
		{"kind": "UserRejectObservation", "tool_call_id": "c2", "rejection_reason": "denied"},
		{"kind": "AgentErrorEvent", "tool_call_id": "c3", "error": "boom"},
		{"kind": "ConversationErrorEvent", "code": huge, "detail": huge},
		{"kind": "PauseEvent"},
		{"kind": "InterruptEvent"},
		{"kind": huge, "callId": "forged", "verdict": "approve"},
		{"kind": "ActionEvent", "tool_call_id": 42, "thought": "not a list", "action": "ls"},
		{"kind": "MessageEvent", "llm_message": map[string]any{"content": text(strings.Repeat("\x02é<", 1<<15))}},
	}
	var raws []string
	for i, ev := range events {
		ev["id"] = "e" + string(rune('a'+i))
		b, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		raws = append(raws, string(b))
	}
	// Malformed events: no id, an id or kind of the wrong type, not an object.
	raws = append(raws, `{"kind":"ActionEvent","tool_call_id":"c9"}`, `{"id":42,"kind":"MessageEvent"}`,
		`{"id":"ez","kind":["ActionEvent"]}`, `[1,2]`, `null`)
	var items []bridge.Mapped
	for _, r := range raws {
		var e bridge.RawEvent
		if err := json.Unmarshal([]byte(r), &e); err != nil {
			t.Fatal(err)
		}
		items = append(items, bridge.Map(e, runA)...)
	}
	var st bridge.StatusTracker
	for _, s := range []string{"idle", "running", "waiting_for_confirmation", "running", "paused", "running", "stuck", huge} {
		items = append(items, st.Observe(s, runA)...)
	}
	for _, m := range items {
		stored, reason := bridgePayload(m.Type, m.Payload)
		if reason != "" {
			t.Errorf("%s refused (%s): %.200s", m.Type, reason, m.Payload)
			continue
		}
		if !bytes.Equal(stored, m.Payload) {
			t.Errorf("%s stored differently from what the bridge sent:\n%.200s\n%.200s", m.Type, m.Payload, stored)
		}
	}
}
