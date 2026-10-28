// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Smana/maku/internal/envelope"
	"github.com/Smana/maku/internal/fanout"
	"github.com/Smana/maku/internal/store"
)

// actorOn gives the server an actor over l, with the test redactor.
func actorOn(l *actLog) option {
	return func(s *Server, _ *fanout.Hub, _ *hubView) {
		s.Actor = &Actor{Log: l, Runs: s.Runs, Groups: groups, Redactor: testRedactor{}}
	}
}

// F8: a note queued through MCP is stored wrapped, under the caller's standing as maku's.
func TestMCPRoomQueue(t *testing.T) {
	l := &actLog{st: store.RoomState{ID: roomID, Driver: "system:factory", DriverEpoch: 3}}
	e := setup(t, mcpOn, actorOn(l))
	got := call(t, e, viaRoomctl("own"), "room_queue", map[string]any{"room": roomID, "text": "use the v2 API"})
	if got.isError || !strings.Contains(string(got.structured), `"queued":1`) {
		t.Fatalf("%+v", got)
	}
	if q := l.queue[0].Text; q != "⟦machine-sent via mcp: a client wrote this on a human's token⟧\nuse the v2 API\n⟦end of machine-sent text⟧" {
		t.Fatalf("queued %q", q)
	}
	if d := l.last(); d.OriginClient == "" || !strings.HasPrefix(d.OriginClient, "human:own:mcp-") {
		t.Fatalf("origin %q", d.OriginClient)
	}
	if got := call(t, e, viaRoomctl("dev"), "room_queue", map[string]any{"room": roomID, "text": "x"}); got.text != "rejected: not_permitted" {
		t.Fatalf("a watcher queues nothing: %+v", got)
	}
}

// room_fork is maku fork's act through MCP: its note is stored wrapped (F8).
func TestMCPRoomFork(t *testing.T) {
	a, l, _, _ := forkFixture(t, nil)
	e := setup(t, mcpOn, func(s *Server, _ *fanout.Hub, _ *hubView) { s.Actor, s.Rooms = a, a.Rooms })
	got := call(t, e, viaRoomctl("bob"), "room_fork", map[string]any{"room": roomID, "at": 2, "note": "try uv"})
	var res struct{ Room, URL string }
	if got.isError || json.Unmarshal(got.structured, &res) != nil || !envelope.ValidID(res.Room) || res.URL != "/r/"+res.Room {
		t.Fatalf("%+v", got)
	}
	var p map[string]any
	_ = json.Unmarshal(l.forked[res.Room].Payload, &p)
	if p["note"] != "⟦machine-sent via mcp: a client wrote this on a human's token⟧\ntry uv\n⟦end of machine-sent text⟧" {
		t.Fatalf("note %v", p["note"])
	}
	if got := call(t, e, viaRoomctl("bob"), "room_fork", map[string]any{"room": roomID, "at": 0}); !strings.HasPrefix(got.text, "invalid_arguments") {
		t.Fatalf("%+v", got)
	}
}
