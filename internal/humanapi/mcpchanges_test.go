// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Smana/agent-platform/internal/envelope"
)

// changesPage is room_changes' structuredContent, as far as its contract goes.
type changesPage struct {
	Room  string
	Items []struct {
		Seq        int64
		Kind, Text string
	}
	Cursor    string
	Untrusted bool
}

func changesOf(t *testing.T, e env, args map[string]any) (changesPage, toolCall) {
	t.Helper()
	got := call(t, e, viaRoomctl("dev"), "room_changes", args)
	var p changesPage
	if got.isError || json.Unmarshal(got.structured, &p) != nil {
		t.Fatalf("%+v", got)
	}
	return p, got
}

// aged is e, stamped d ago.
func aged(e envelope.Event, d time.Duration) envelope.Event {
	e.TS = time.Now().Add(-d)
	return e
}

func taskFacts(seq int64, f envelope.TaskFacts) envelope.Event {
	return logEvent(seq, envelope.StateChanged, envelope.Actor{Kind: envelope.ActorSystem, ID: "system:factory"}, envelope.TaskStatePayload(f))
}

func TestMCPRoomChanges(t *testing.T) {
	e := setup(t, mcpOn)
	e.log.evs = []envelope.Event{progress(1, "planning"), progress(2, "tests written"), progress(3, "checks pass")}

	p, got := changesOf(t, e, map[string]any{"room": roomID})
	if !strings.HasPrefix(got.text, "**"+roomID+" · since") || len(p.Items) != 3 || !p.Untrusted || p.Room != roomID || p.Cursor != "seq:3" {
		t.Fatalf("no cursor: %q %+v", got.text, p)
	}
	p, _ = changesOf(t, e, map[string]any{"room": roomID, "cursor": "seq:1"})
	if len(p.Items) != 2 || p.Items[0].Seq != 2 || p.Cursor != "seq:3" {
		t.Fatalf("cursor seq:1: %+v", p)
	}
	// The cursor passed back is an empty page, and its items are an array, never null.
	p, got = changesOf(t, e, map[string]any{"room": roomID, "cursor": p.Cursor})
	if len(p.Items) != 0 || p.Cursor != "seq:3" || !strings.Contains(string(got.structured), `"items":[]`) || !strings.Contains(got.text, "Nothing changed") {
		t.Fatalf("round trip: %s %q", got.structured, got.text)
	}
	e.log.add(1) // a chat message moves the log but is no milestone
	if p, _ = changesOf(t, e, map[string]any{"room": roomID, "cursor": "seq:3"}); len(p.Items) != 0 {
		t.Fatalf("a chat message is no milestone: %+v", p)
	}
}

func TestMCPRoomChangesBaseline(t *testing.T) {
	pr := &envelope.PRFact{Number: 7, URL: "https://github.com/Smana/a/pull/7"}
	kinds := func(p changesPage) string {
		var ks []string
		for _, it := range p.Items {
			ks = append(ks, it.Kind)
		}
		return strings.Join(ks, ",")
	}
	e := setup(t, mcpOn)
	e.log.evs = []envelope.Event{
		taskFacts(1, envelope.TaskFacts{Phase: "Implementing", PR: pr}),
		taskFacts(2, envelope.TaskFacts{Phase: "AwaitingHuman", PR: pr}),
	}
	if p, _ := changesOf(t, e, map[string]any{"room": roomID, "cursor": "seq:1"}); strings.Contains(kinds(p), "pr_opened") {
		t.Fatalf("a PR at or below the cursor is not new: %s", kinds(p))
	}
	e.log.evs = []envelope.Event{
		taskFacts(1, envelope.TaskFacts{Phase: "Implementing"}),
		taskFacts(2, envelope.TaskFacts{Phase: "AwaitingHuman", PR: pr}),
	}
	if p, _ := changesOf(t, e, map[string]any{"room": roomID, "cursor": "seq:1"}); !strings.Contains(kinds(p), "pr_opened") {
		t.Fatalf("a PR after the cursor is new: %s", kinds(p))
	}
}

func TestMCPRoomChangesLast24h(t *testing.T) {
	e := setup(t, mcpOn)
	e.log.evs = []envelope.Event{
		aged(progress(1, "two days ago"), 48*time.Hour),
		aged(progress(2, "this morning"), time.Hour),
	}
	p, _ := changesOf(t, e, map[string]any{"room": roomID})
	if len(p.Items) != 1 || p.Items[0].Seq != 2 {
		t.Fatalf("without a cursor only the last 24 h: %+v", p)
	}
	// A cursor reads from there, however old.
	if p, _ = changesOf(t, e, map[string]any{"room": roomID, "cursor": "seq:0"}); len(p.Items) != 2 {
		t.Fatalf("seq:0 reads it all: %+v", p)
	}
}

func TestMCPRoomChangesRefusals(t *testing.T) {
	e := setup(t, mcpOn)
	if got := call(t, e, viaRoomctl("dev"), "room_changes", map[string]any{"room": "aaaaaaaa"}); !got.isError || got.text != "no such room" {
		t.Fatalf("a missing room: %+v", got)
	}
	for name, args := range map[string]map[string]any{
		"a bad cursor":     {"room": roomID, "cursor": "seq:x"},
		"no room":          {"cursor": "seq:1"},
		"an unknown field": {"room": roomID, "after": "seq:1"},
	} {
		if got := call(t, e, viaRoomctl("dev"), "room_changes", args); !got.isError || !strings.HasPrefix(got.text, "invalid_arguments") {
			t.Errorf("%s: %+v", name, got)
		}
	}
	// A room the caller cannot read answers as a missing one does.
	w := world(map[string][]string{"Smana/a": {"dev1"}})
	e = setup(t, mcpOn, abc(t), w.option())
	for _, room := range []string{roomB, roomC} {
		if got := call(t, e, viaRoomctl("dev1"), "room_changes", map[string]any{"room": room}); !got.isError || got.text != "no such room" {
			t.Errorf("room %s: %+v", room, got)
		}
	}
}

func TestSponsoredSeesRoomChanges(t *testing.T) {
	e := setup(t, mcpOn, withSponsored(sponsoredP))
	if got := toolNames(t, e, bearer("slack-app")); !strings.Contains(","+got+",", ",room_changes,") {
		t.Fatalf("%s", got)
	}
}
