// SPDX-License-Identifier: Apache-2.0

package envelope

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestValidID(t *testing.T) {
	for id, want := range map[string]bool{
		"3kq7x2ma": true, "abcdefgh": true, "7f3cq2xz": true,
		"3KQ7X2MA": false, "3kq7x2m": false, "3kq7x2ma1": false, "3kq7x2m0": false, "3kq7x2m1": false,
	} {
		if got := ValidID(id); got != want {
			t.Errorf("ValidID(%q) = %v, want %v", id, got, want)
		}
	}
}

func TestEventCarriesExactlyTheC4Keys(t *testing.T) {
	caused := int64(1840)
	ev := Event{V: 1, ID: "01J9X7K2", Seq: 1842, RoomID: "3kq7x2ma", RunID: "7f3cq2xz",
		Actor: Actor{Kind: ActorAgent, ID: "agent:7f3cq2xz", Role: "reviewer"}, Type: Message,
		CausedBy: &caused, Origin: OriginHarness, TS: time.Unix(0, 0).UTC(), Redactions: []string{},
		Payload: json.RawMessage(`{}`)}
	raw, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	want := []string{"actor", "causedBy", "id", "origin", "payload", "redactions", "roomId", "runId", "seq", "ts", "type", "v"}
	if !slices.Equal(keys, want) {
		t.Fatalf("keys = %v, want %v", keys, want)
	}
}

func TestDraftValidate(t *testing.T) {
	ok := Draft{RoomID: "3kq7x2ma", Actor: Actor{Kind: ActorSystem, ID: "system:room-broker"},
		Type: StateChanged, Origin: OriginBroker, OriginClient: "broker:room", OriginSeq: 1,
		Payload: StatePayload("room_phase", map[string]any{"phase": "Open"})}
	if err := ok.Validate(); err != nil {
		t.Fatalf("valid draft refused: %v", err)
	}
	for name, mutate := range map[string]func(*Draft){
		"bad room":      func(d *Draft) { d.RoomID = "ROOM" },
		"bad run":       func(d *Draft) { d.RunID = "x" },
		"bad type":      func(d *Draft) { d.Type = "chat" },
		"no actor":      func(d *Draft) { d.Actor.ID = "" },
		"bad origin":    func(d *Draft) { d.Origin = "ui" },
		"no key":        func(d *Draft) { d.OriginClient = "" },
		"bad payload":   func(d *Draft) { d.Payload = json.RawMessage(`{`) },
		"empty payload": func(d *Draft) { d.Payload = nil },
	} {
		d := ok
		mutate(&d)
		if d.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestPayloadFieldNamesFollowAppendixA(t *testing.T) {
	raw := string(Must(ToolCallPayload{CallID: "c1", Tool: "terminal", Args: json.RawMessage(`{}`)}))
	for _, k := range []string{`"callId"`, `"tool"`, `"args"`, `"decidedBy":null`} {
		if !strings.Contains(raw, k) {
			t.Errorf("tool_call payload %s lacks %s", raw, k)
		}
	}
	if raw := string(Must(HandoffPayload{FromRole: "implementer", ToRole: "reviewer"})); !strings.Contains(raw, `"fromRole"`) || !strings.Contains(raw, `"toRole"`) {
		t.Errorf("handoff payload %s", raw)
	}
	if raw := string(StatePayload("run_phase", map[string]any{"phase": "Failed"})); raw != `{"kind":"run_phase","phase":"Failed"}` {
		t.Errorf("state_changed = %s", raw)
	}
	if raw := string(Oversize(ToolResult, 70000)); raw != `{"bytes":70000,"oversize":true,"type":"tool_result"}` {
		t.Errorf("oversize = %s", raw)
	}
}
