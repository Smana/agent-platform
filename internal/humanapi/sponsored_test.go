// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/fanout"
)

var sponsoredP = authn.Principal{Kind: envelope.ActorSystem, ID: "system:slack-app", Sponsored: true, AccessToken: "t",
	Expiry: time.Now().Add(24 * time.Hour)}

// sponsoredTokens is the broker's sponsored authenticator in the tests: a bearer names its principal.
type sponsoredTokens map[string]authn.Principal

func (m sponsoredTokens) Authenticate(_ context.Context, raw string) (authn.Principal, error) {
	if p, ok := m[raw]; ok {
		return p, nil
	}
	return authn.Principal{}, authn.ErrUnauthenticated
}

// withSponsored declares ps on the broker; each one's bearer is its name after system:.
func withSponsored(ps ...authn.Principal) option {
	return func(s *Server, _ *fanout.Hub, _ *hubView) {
		m := sponsoredTokens{}
		for _, p := range ps {
			m[strings.TrimPrefix(p.ID, "system:")] = p
		}
		s.Sponsored = m
	}
}

// bearer is a sponsored principal's request headers.
func bearer(name string) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+name)
	return h
}

// sponsoredRooms serves room A (roomID) with system:slack-app a watcher and room B without it, on
// w's GitHub, whose clock counts every access check.
func sponsoredRooms(t *testing.T, w *gitHubWorld) option {
	return func(s *Server, _ *fanout.Hub, _ *hubView) {
		mine := repoRoom(roomID, "Smana/a")
		mine.Spec.Members = []v1alpha1.Member{{Principal: "system:slack-app", Role: "watcher"}}
		s.Rooms = roomClient(t, mine, repoRoom(roomB, "Smana/b"))
		w.gate(s)
	}
}

// toolNames is tools/list's names, in order, as h sees them.
func toolNames(t *testing.T, e env, h http.Header) string {
	t.Helper()
	_, out := rpc(t, e, h, "tools/list", nil)
	var res struct{ Tools []struct{ Name string } }
	b, _ := json.Marshal(out["result"])
	_ = json.Unmarshal(b, &res)
	names := make([]string, 0, len(res.Tools))
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	return strings.Join(names, ",")
}

// R13, invariant 8: a sponsored principal's tools read, and none labels, approves, steers, queues,
// forks or stops; a call to a human's tool is an unknown tool (C2).
func TestSponsoredToolList(t *testing.T) {
	e := setup(t, mcpOn, withSponsored(sponsoredP))
	if got := toolNames(t, e, bearer("slack-app")); got != "issue_draft,room_summary,room_tail" {
		t.Fatalf("%s", got)
	}
	if got := toolNames(t, e, viaRoomctl("dev")); got != "rooms_list,room_summary,room_tail,room_queue,room_fork,issue_draft" {
		t.Fatalf("a human's list changed: %s", got)
	}
	for _, name := range []string{"room_queue", "room_fork", "rooms_list"} {
		_, out := rpc(t, e, bearer("slack-app"), "tools/call", map[string]any{"name": name,
			"arguments": map[string]any{"room": roomID, "text": "x", "at": 1}})
		if rpcCode(out) != -32602 || !strings.Contains(fmt.Sprint(out["error"]), "unknown tool: "+name) {
			t.Errorf("%s: %v", name, out)
		}
	}
}

// R13: a sponsored principal reads a room only as its member, never through GitHub, never another.
func TestSponsoredReadsItsRoomsOnly(t *testing.T) {
	w := world(nil)
	e := setup(t, mcpOn, withSponsored(sponsoredP), sponsoredRooms(t, w))
	if got := call(t, e, bearer("slack-app"), "room_summary", map[string]any{"room": roomID}); got.isError {
		t.Fatalf("its own room: %s", got.text)
	}
	if got := call(t, e, bearer("slack-app"), "room_summary", map[string]any{"room": roomB}); !got.isError || !strings.Contains(got.text, noSuchRoom) {
		t.Fatalf("another room: %+v", got)
	}
	if n := w.checked(); n != 0 {
		t.Fatalf("a sponsored read asked GitHub %d times", n)
	}
}

// R13, C18: a sponsored token is unauthenticated everywhere but /mcp, and on /mcp while no
// principal is declared: principal() runs the human authenticator, which never accepts it.
func TestSponsoredTokenElsewhere(t *testing.T) {
	e := setup(t, mcpOn, withSponsored(sponsoredP))
	for _, path := range []string{"/api/rooms", "/api/rooms/" + roomID + "/summary", "/v1/ws?room=" + roomID} {
		if code, _ := get(t, e, path, bearer("slack-app")); code != http.StatusUnauthorized {
			t.Errorf("%s: %d", path, code)
		}
	}
	off := setup(t, mcpOn)
	if code, _ := rpc(t, off, bearer("slack-app"), "tools/list", nil); code != http.StatusUnauthorized {
		t.Fatalf("no principal declared: %d", code)
	}
}

// C28: the instructions are built per principal: a sponsored one is not told of a developer's
// room page, roomctl or labels, and a human's text is unchanged.
func TestSponsoredInstructions(t *testing.T) {
	e := setup(t, mcpOn, withSponsored(sponsoredP))
	text := func(h http.Header) string {
		_, out := rpc(t, e, h, "initialize", map[string]any{"protocolVersion": "2025-11-25"})
		return fmt.Sprint(out["result"].(map[string]any)["instructions"])
	}
	human, sponsored := text(viaRoomctl("dev")), text(bearer("slack-app"))
	if human != mcpInstructions || sponsored == human || strings.Contains(sponsored, "roomctl") ||
		!strings.Contains(sponsored, "untrusted: true") {
		t.Fatalf("human %q, sponsored %q", human, sponsored)
	}
}
