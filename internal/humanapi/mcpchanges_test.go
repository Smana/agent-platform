// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Smana/maku/internal/envelope"
	"github.com/Smana/maku/internal/fanout"
	"github.com/Smana/maku/internal/roomctrl"
)

// changesPage is room_changes' structuredContent, as far as its contract goes.
type changesPage struct {
	Room  string
	Items []struct {
		Seq        int64
		Kind, Text string
	}
	Cursor    string
	More      int
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

// The scan reads at most tailScan events a call: when it stops before the end of the log it says
// so with more > 0, and the cursor it returns carries on from there until the log is read.
func TestMCPRoomChangesScanBound(t *testing.T) {
	e := setup(t, mcpOn)
	e.log.add(tailScan + 490) // chat messages: no milestone, so only the scan's bound ends the page
	last := int64(len(e.log.evs))
	p, _ := changesOf(t, e, map[string]any{"room": roomID, "cursor": "seq:0"})
	if p.Cursor != "seq:"+strconv.Itoa(tailScan) || p.More == 0 || len(p.Items) != 0 {
		t.Fatalf("a stop at the bound: %+v", p)
	}
	p, _ = changesOf(t, e, map[string]any{"room": roomID, "cursor": p.Cursor})
	if p.Cursor != "seq:"+strconv.FormatInt(last, 10) || p.More != 0 {
		t.Fatalf("the rest: %+v", p)
	}
	// A cursor past the end is clamped to the last seq.
	if p, _ = changesOf(t, e, map[string]any{"room": roomID, "cursor": "seq:99999"}); p.Cursor != "seq:"+strconv.FormatInt(last, 10) || p.More != 0 {
		t.Fatalf("clamped: %+v", p)
	}
}

// Without a cursor the facts older than the window are the baseline of what follows.
func TestMCPRoomChangesBaselineAfterTheTrim(t *testing.T) {
	pr := &envelope.PRFact{Number: 7, URL: "https://github.com/Smana/a/pull/7"}
	has := func(p changesPage, kind string) bool {
		for _, it := range p.Items {
			if it.Kind == kind {
				return true
			}
		}
		return false
	}
	e := setup(t, mcpOn)
	e.log.evs = []envelope.Event{
		aged(taskFacts(1, envelope.TaskFacts{Phase: "Implementing"}), 48*time.Hour),
		aged(taskFacts(2, envelope.TaskFacts{Phase: "AwaitingHuman", PR: pr}), 47*time.Hour),
		taskFacts(3, envelope.TaskFacts{Phase: "AwaitingCI", PR: pr}),
	}
	if p, _ := changesOf(t, e, map[string]any{"room": roomID}); has(p, "pr_opened") {
		t.Fatalf("the PR was open before the window: %+v", p)
	}
	e.log.evs[1] = aged(taskFacts(2, envelope.TaskFacts{Phase: "Implementing"}), 47*time.Hour)
	if p, _ := changesOf(t, e, map[string]any{"room": roomID}); !has(p, "pr_opened") {
		t.Fatalf("the PR opened inside the window: %+v", p)
	}
}

// A fork's log begins with its source's prefix: those events are not the fork's milestones.
func TestMCPRoomChangesSkipsAForksPrefix(t *testing.T) {
	forked := repoRoom(roomID, "Smana/a")
	forked.Annotations = map[string]string{roomctrl.ForkedFrom: "3kq7x2md@3"}
	pr := &envelope.PRFact{Number: 7, URL: "https://github.com/Smana/a/pull/7"}
	e := setup(t, mcpOn, func(s *Server, _ *fanout.Hub, _ *hubView) { s.Rooms = roomClient(t, forked) })
	e.log.evs = []envelope.Event{
		taskFacts(1, envelope.TaskFacts{Phase: "Implementing"}),
		progress(2, "the source's note"),
		taskFacts(3, envelope.TaskFacts{Phase: "AwaitingHuman", PR: pr}),
		progress(4, "the fork's note"),
	}
	for _, args := range []map[string]any{{"room": roomID}, {"room": roomID, "cursor": "seq:0"}, {"room": roomID, "cursor": "seq:1"}} {
		p, _ := changesOf(t, e, args)
		for _, it := range p.Items {
			if it.Seq <= 3 {
				t.Errorf("%v: the source's seq %d shows as the fork's: %+v", args, it.Seq, it)
			}
		}
		if len(p.Items) == 0 {
			t.Errorf("%v: the fork's own note is missing", args)
		}
	}
}

// A sponsored principal reads a room's changes only as its member.
func TestSponsoredRoomChangesStaysInItsRooms(t *testing.T) {
	e := setup(t, mcpOn, withSponsored(sponsoredP), sponsoredRooms(t, world(nil)))
	if got := call(t, e, bearer("slack-app"), "room_changes", map[string]any{"room": roomID}); got.isError {
		t.Fatalf("its own room: %s", got.text)
	}
	if got := call(t, e, bearer("slack-app"), "room_changes", map[string]any{"room": roomB}); !got.isError || got.text != "no such room" {
		t.Fatalf("another room: %+v", got)
	}
}
