// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/fanout"
)

// toolCall is one tools/call's result as the model reads it.
type toolCall struct {
	structured json.RawMessage
	isError    bool
	text       string
}

// call sends tools/call name(args) to /mcp with h.
func call(t *testing.T, e env, h http.Header, name string, args any) toolCall {
	t.Helper()
	_, out := rpc(t, e, h, "tools/call", map[string]any{"name": name, "arguments": args})
	b, _ := json.Marshal(out["result"])
	var r struct {
		Structured json.RawMessage `json:"structuredContent"`
		IsError    bool            `json:"isError"`
		Content    []struct{ Text string }
	}
	if json.Unmarshal(b, &r) != nil || len(r.Content) != 1 {
		t.Fatalf("%s: not a tool result: %v", name, out)
	}
	return toolCall{structured: r.Structured, isError: r.IsError, text: r.Content[0].Text}
}

func TestMCPRoomsList(t *testing.T) {
	waiting := repoRoom(roomID, "Smana/a")
	waiting.Status.Phase, waiting.Status.Task = "Idle", &v1alpha1.TaskStatus{Phase: "AwaitingHuman", Issue: 42}
	e := setup(t, mcpOn, func(s *Server, _ *fanout.Hub, _ *hubView) { s.Rooms = roomClient(t, waiting) })
	e.log.mu.Lock()
	e.log.evs = append(e.log.evs, logEvent(11, envelope.StateChanged, envelope.Actor{Kind: envelope.ActorSystem, ID: "system:factory"},
		envelope.TaskStatePayload(envelope.TaskFacts{Phase: "AwaitingHuman", Reason: "merge_blocked"})))
	e.log.mu.Unlock()
	got := call(t, e, viaRoomctl("dev"), "rooms_list", map[string]any{"repo": "Smana/a"})
	var res struct {
		Rooms []struct {
			ID, State, Phase, Task, Reason, URL string
		}
		Access string
	}
	if got.isError || json.Unmarshal(got.structured, &res) != nil || len(res.Rooms) != 1 {
		t.Fatalf("%+v", got)
	}
	if r := res.Rooms[0]; r.State != "waiting_for_human" || r.Task != "AwaitingHuman" || r.Reason != "merge_blocked" || r.URL != "/r/"+roomID || res.Access != "ok" {
		t.Fatalf("%+v %s", r, res.Access)
	}
	if got := call(t, e, viaRoomctl("dev"), "rooms_list", map[string]any{"repo": "not a repo"}); !got.isError || !strings.HasPrefix(got.text, "invalid_arguments") {
		t.Fatalf("%+v", got)
	}
}

func TestMCPRoomSummary(t *testing.T) {
	e := setup(t, mcpOn)
	got := call(t, e, viaRoomctl("dev"), "room_summary", map[string]any{"room": roomID, "after": "seq:3"})
	var s struct {
		APIVersion string
		Status     struct{ State string }
		Notes      struct{ Untrusted bool }
	}
	if got.isError || json.Unmarshal(got.structured, &s) != nil || s.APIVersion != "summary/v1" || s.Status.State != "running" || !s.Notes.Untrusted {
		t.Fatalf("%+v", got)
	}
	for name, args := range map[string]map[string]any{
		"a missing room":   {"room": "aaaaaaaa"},
		"a bad cursor":     {"room": roomID, "after": "seq:x"},
		"an unknown field": {"room": roomID, "approve": true},
	} {
		if got := call(t, e, viaRoomctl("dev"), "room_summary", args); !got.isError {
			t.Errorf("%s: %+v", name, got)
		}
	}
	if got := call(t, e, viaRoomctl("dev"), "room_summary", map[string]any{"room": "aaaaaaaa"}); got.text != "no such room" {
		t.Fatalf("a missing room reads as an unreadable one: %q", got.text)
	}
}

func TestMCPRoomTail(t *testing.T) {
	e := setup(t, mcpOn)
	e.log.add(50) // 60 events
	page := func(args map[string]any) tailPage {
		t.Helper()
		got := call(t, e, viaRoomctl("dev"), "room_tail", args)
		var p tailPage
		if got.isError || json.Unmarshal(got.structured, &p) != nil {
			t.Fatalf("%+v", got)
		}
		return p
	}
	p := page(map[string]any{"room": roomID})
	if len(p.Events.Items) != 50 || p.Events.Items[0].Seq != 11 || p.Cursor != "seq:60" || p.More || !p.Events.Untrusted || p.State != "running" {
		t.Fatalf("no cursor: the last 50: %d from %d, %s more=%v", len(p.Events.Items), p.Events.Items[0].Seq, p.Cursor, p.More)
	}
	if p = page(map[string]any{"room": roomID, "cursor": "seq:0"}); len(p.Events.Items) != 60 || p.Cursor != "seq:60" {
		t.Fatalf("from 0: %d %s", len(p.Events.Items), p.Cursor)
	}
	e.log.add(100) // 160 events
	if p = page(map[string]any{"room": roomID, "cursor": "seq:0"}); len(p.Events.Items) != tailMax || p.Cursor != "seq:100" || !p.More {
		t.Fatalf("a page holds 100: %d %s more=%v", len(p.Events.Items), p.Cursor, p.More)
	}
	if p = page(map[string]any{"room": roomID, "cursor": "seq:150", "kinds": []string{"handoff"}}); len(p.Events.Items) != 0 || p.Cursor != "seq:160" || p.More {
		t.Fatalf("kinds: %d %s", len(p.Events.Items), p.Cursor)
	}
	if got := call(t, e, viaRoomctl("dev"), "room_tail", map[string]any{"room": roomID, "kinds": []string{"secrets"}}); !got.isError {
		t.Fatalf("an unknown kind: %+v", got)
	}
}

func TestViewToolsReturnTextAndStructure(t *testing.T) {
	e := setup(t, mcpOn)
	list := call(t, e, viaRoomctl("dev"), "rooms_list", map[string]any{})
	if !strings.HasPrefix(list.text, "**Factory rooms**") {
		t.Fatalf("rooms_list text: %q", list.text)
	}
	var l struct{ Rooms []json.RawMessage }
	if json.Unmarshal(list.structured, &l) != nil || len(l.Rooms) == 0 {
		t.Fatalf("rooms_list structure: %s", list.structured)
	}
	sum := call(t, e, viaRoomctl("dev"), "room_summary", map[string]any{"room": roomID})
	if !strings.HasPrefix(sum.text, "**") || !strings.Contains(sum.text, "\n\n### ") { // the status line, then the heading
		t.Fatalf("room_summary text: %q", sum.text)
	}
	var s struct{ APIVersion string }
	if json.Unmarshal(sum.structured, &s) != nil || s.APIVersion != "summary/v1" {
		t.Fatalf("room_summary structure: %s", sum.structured)
	}
}

func TestViewToolsDeclareOutputSchema(t *testing.T) {
	e := setup(t, mcpOn)
	_, out := rpc(t, e, viaRoomctl("dev"), "tools/list", nil)
	b, _ := json.Marshal(out["result"])
	var r struct {
		Tools []struct {
			Name         string
			OutputSchema *struct {
				Type     string
				Required []string
			}
		}
	}
	if json.Unmarshal(b, &r) != nil {
		t.Fatal(string(b))
	}
	calls := map[string]toolCall{
		"rooms_list":   call(t, e, viaRoomctl("dev"), "rooms_list", map[string]any{}),
		"room_summary": call(t, e, viaRoomctl("dev"), "room_summary", map[string]any{"room": roomID}),
		"room_changes": call(t, e, viaRoomctl("dev"), "room_changes", map[string]any{"room": roomID}),
	}
	seen := 0
	for _, tool := range r.Tools {
		got, view := calls[tool.Name]
		if !view {
			if tool.OutputSchema != nil {
				t.Errorf("%s declares an outputSchema", tool.Name)
			}
			continue
		}
		seen++
		if tool.OutputSchema == nil || tool.OutputSchema.Type != "object" || len(tool.OutputSchema.Required) == 0 {
			t.Fatalf("%s: outputSchema %+v", tool.Name, tool.OutputSchema)
		}
		var keys map[string]json.RawMessage
		if json.Unmarshal(got.structured, &keys) != nil {
			t.Fatal(string(got.structured))
		}
		for _, k := range tool.OutputSchema.Required {
			if _, ok := keys[k]; !ok {
				t.Errorf("%s requires %q, which the structured result lacks", tool.Name, k)
			}
		}
	}
	if seen != 3 {
		t.Fatalf("saw %d view tools", seen)
	}
}

func TestRoomsListAllOnlyChangesTheText(t *testing.T) {
	old := repoRoom(roomID, "Smana/a")
	old.Status.Phase, old.Status.Task = "Closed", &v1alpha1.TaskStatus{Phase: "Merged", Issue: 42}
	e := setup(t, mcpOn, func(s *Server, _ *fanout.Hub, _ *hubView) { s.Rooms = roomClient(t, old) })
	e.log.mu.Lock()
	e.log.evs = append(e.log.evs, logEvent(11, envelope.StateChanged, envelope.Actor{Kind: envelope.ActorSystem, ID: "system:factory"},
		envelope.TaskStatePayload(envelope.TaskFacts{Phase: "Merged"})))
	e.log.lastEventAt = map[string]time.Time{roomID: time.Now().Add(-30 * 24 * time.Hour)}
	e.log.mu.Unlock()
	for _, all := range []bool{false, true} {
		got := call(t, e, viaRoomctl("dev"), "rooms_list", map[string]any{"all": all})
		var res struct{ Rooms []struct{ ID string } }
		if json.Unmarshal(got.structured, &res) != nil || len(res.Rooms) != 1 {
			t.Fatalf("all=%v: structured must hold every row: %s", all, got.structured)
		}
		if has := strings.Contains(got.text, "#42"); has != all {
			t.Errorf("all=%v: text shows the old room = %v\n%s", all, has, got.text)
		}
	}
}

// The nested objects of an outputSchema name their required keys, and a real result has them all.
func TestViewToolsOutputSchemaNestedRequired(t *testing.T) {
	e := setup(t, mcpOn)
	e.log.evs = []envelope.Event{progress(1, "planning")}
	_, out := rpc(t, e, viaRoomctl("dev"), "tools/list", nil)
	b, _ := json.Marshal(out["result"])
	var r struct {
		Tools []struct {
			Name         string
			OutputSchema struct {
				Properties map[string]struct {
					Type     string
					Required []string
					Items    *struct {
						Type     string
						Required []string
					}
				}
			}
		}
	}
	if json.Unmarshal(b, &r) != nil {
		t.Fatal(string(b))
	}
	calls := map[string]toolCall{
		"rooms_list":   call(t, e, viaRoomctl("dev"), "rooms_list", map[string]any{}),
		"room_summary": call(t, e, viaRoomctl("dev"), "room_summary", map[string]any{"room": roomID}),
		"room_changes": call(t, e, viaRoomctl("dev"), "room_changes", map[string]any{"room": roomID, "cursor": "seq:0"}),
	}
	// the nested objects each tool must constrain
	nested := map[string][]string{"rooms_list": {"rooms"}, "room_summary": {"status"}, "room_changes": {"items"}}
	for _, tool := range r.Tools {
		got, ok := calls[tool.Name]
		if !ok {
			continue
		}
		var data map[string]json.RawMessage
		if json.Unmarshal(got.structured, &data) != nil {
			t.Fatal(string(got.structured))
		}
		for _, name := range nested[tool.Name] {
			prop := tool.OutputSchema.Properties[name]
			required, elems := prop.Required, []json.RawMessage{data[name]}
			if prop.Type == "array" {
				if prop.Items == nil {
					t.Fatalf("%s.%s: an array with no items schema", tool.Name, name)
				}
				required = prop.Items.Required
				elems = nil
				_ = json.Unmarshal(data[name], &elems)
			}
			if len(required) == 0 || len(elems) == 0 {
				t.Errorf("%s.%s: required %v over %d values", tool.Name, name, required, len(elems))
			}
			for _, el := range elems {
				var keys map[string]json.RawMessage
				_ = json.Unmarshal(el, &keys)
				for _, k := range required {
					if _, ok := keys[k]; !ok {
						t.Errorf("%s.%s requires %q, which %s lacks", tool.Name, name, k, el)
					}
				}
			}
		}
	}
}
