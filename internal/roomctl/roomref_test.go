// SPDX-License-Identifier: Apache-2.0

package roomctl

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// O7: a room is named the way the developer says it, #<issue>, PR #<n> or a bare number, and
// resolved through the room list; the room id still works.
var refRooms = []map[string]any{
	{"id": "abcdefgh", "repository": "Smana/a", "issue": 7, "title": "docs: a dead link", "pr": 12},
	{"id": "bcdefghi", "repository": "Smana/a", "issue": 12, "title": "fix: the other one", "pr": 0},
	{"id": "cdefghij", "repository": "Smana/b", "issue": 30, "title": "feat: thirty", "pr": 7},
	{"id": "defghijk", "repository": "Smana/c", "issue": 40, "title": "chore: forty", "pr": 0},
	{"id": "efghijkl", "repository": "Smana/d", "issue": 40, "title": "chore: forty again", "pr": 0},
}

func lastCall(t *testing.T, b *mcpBroker) toolCall {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.calls) == 0 {
		return toolCall{}
	}
	return b.calls[len(b.calls)-1]
}

func TestRoomReferencesResolve(t *testing.T) {
	for name, tc := range map[string]struct{ ref, want string }{
		"id as is":                  {"abcdefgh", "abcdefgh"},
		"#issue":                    {"#7", "abcdefgh"},
		"bare number is the issue":  {"12", "bcdefghi"}, // issue 12 and PR 12 both exist: the issue wins
		"#number prefers the issue": {"#12", "bcdefghi"},
		"PR #n":                     {"PR #12", "abcdefgh"},
		"pr n lower case":           {"pr 7", "cdefghij"},
	} {
		t.Run(name, func(t *testing.T) {
			b := newMCPBroker(t)
			b.rooms = refRooms
			s := startSession(t, b, &filing{}, &labeling{}, time.Second)
			res := s.request(t, 5, "tools/call", map[string]any{"name": "room_summary", "arguments": map[string]any{"room": tc.ref}})
			if r := res["result"].(map[string]any); r["isError"] == true {
				t.Fatalf("%v", r)
			}
			if got := lastCall(t, b); got.Name != "room_summary" || got.Args["room"] != tc.want {
				t.Fatalf("the broker got %+v, want room %s", got, tc.want)
			}
		})
	}
}

// With no issue of that number, a PR of it is the room.
func TestRoomReferenceFallsBackToThePR(t *testing.T) {
	b := newMCPBroker(t)
	b.rooms = []map[string]any{{"id": "cdefghij", "repository": "Smana/b", "issue": 30, "title": "feat", "pr": 7}}
	s := startSession(t, b, &filing{}, &labeling{}, time.Second)
	s.request(t, 5, "tools/call", map[string]any{"name": "room_summary", "arguments": map[string]any{"room": "#7"}})
	if got := lastCall(t, b); got.Args["room"] != "cdefghij" {
		t.Fatalf("%+v", got)
	}
}

// Several rooms match: the developer is asked, nothing is guessed, and the broker is not called.
func TestRoomReferenceAmbiguousAsksAndNeverGuesses(t *testing.T) {
	for _, tool := range []string{"room_summary", "room_changes", "room_queue", "room_checkout"} {
		t.Run(tool, func(t *testing.T) {
			b := newMCPBroker(t)
			b.rooms = refRooms
			s := startSession(t, b, &filing{}, &labeling{}, time.Second)
			args := map[string]any{"room": "#40", "text": "hi"}
			res := s.request(t, 5, "tools/call", map[string]any{"name": tool, "arguments": args})["result"].(map[string]any)
			text := fmt.Sprint(res["content"])
			if res["isError"] != true || !strings.Contains(text, "ambiguous_room") || !strings.Contains(text, "defghijk") ||
				!strings.Contains(text, "efghijkl") || !strings.Contains(text, "Smana/c") || !strings.Contains(strings.ToLower(text), "ask") {
				t.Fatalf("%v", res)
			}
			if got := lastCall(t, b); got.Name == tool {
				t.Fatalf("the broker was called with %+v", got)
			}
		})
	}
}

func TestRoomReferenceUnknown(t *testing.T) {
	b := newMCPBroker(t)
	b.rooms = refRooms
	s := startSession(t, b, &filing{}, &labeling{}, time.Second)
	for ref, want := range map[string]string{"#999": "unknown_room", "PR #999": "unknown_room"} {
		res := s.request(t, 5, "tools/call", map[string]any{"name": "room_summary", "arguments": map[string]any{"room": ref}})["result"].(map[string]any)
		if res["isError"] != true || !strings.Contains(fmt.Sprint(res["content"]), want) {
			t.Fatalf("%q: %v", ref, res)
		}
	}
	if got := lastCall(t, b); got.Name == "room_summary" {
		t.Fatalf("the broker was called with %+v", got)
	}
}

// room_wait takes references in its rooms list too.
func TestRoomWaitResolvesReferences(t *testing.T) {
	b := newMCPBroker(t)
	b.rooms = []map[string]any{{"id": "abcdefgh", "repository": "Smana/a", "issue": 7, "title": "t", "state": "done", "phase": "Sealed"}}
	s := startSession(t, b, &filing{}, &labeling{}, time.Second)
	res := s.request(t, 5, "tools/call", map[string]any{"name": "room_wait", "arguments": map[string]any{"rooms": []string{"#7"}, "timeout": 1}})["result"].(map[string]any)
	if res["isError"] == true && strings.Contains(fmt.Sprint(res["content"]), "invalid_arguments") {
		t.Fatalf("%v", res)
	}
}

// The tools' schemas no longer pin the room to an id, or a client validating them would refuse #7.
func TestToolsListAcceptsReferences(t *testing.T) {
	b := newMCPBroker(t)
	s := startSession(t, b, &filing{}, &labeling{}, time.Second)
	res := s.request(t, 5, "tools/list", map[string]any{})["result"].(map[string]any)
	for _, raw := range res["tools"].([]any) {
		tool := raw.(map[string]any)
		if !strings.HasPrefix(fmt.Sprint(tool["name"]), "room_") {
			continue
		}
		if strings.Contains(fmt.Sprint(tool["inputSchema"]), "[a-z2-7]") {
			t.Errorf("%s still pins the room to an id: %v", tool["name"], tool["inputSchema"])
		}
		if !strings.Contains(fmt.Sprint(tool["description"]), "#<issue>") {
			t.Errorf("%s does not say it takes #<issue>: %v", tool["name"], tool["description"])
		}
	}
}
