// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/fanout"
)

func logEvent(seq int64, typ envelope.Type, actor envelope.Actor, payload any) envelope.Event {
	return envelope.Event{V: 1, Seq: seq, RoomID: roomID, Type: typ, Actor: actor, TS: time.Now(), Payload: envelope.Must(payload)}
}

func progress(seq int64, text string) envelope.Event {
	return logEvent(seq, envelope.Message, envelope.Actor{Kind: envelope.ActorAgent, ID: "agent:x", Role: "implementer"},
		envelope.MessagePayload{Kind: envelope.KindProgress, Text: text, Delivery: envelope.DeliveryNone})
}

// factsLog is a room with task facts, two progress notes and a pending approval.
func factsLog(l *memLog) {
	l.evs = []envelope.Event{
		{V: 1, Seq: 1, RoomID: roomID, Type: envelope.StateChanged, TS: time.Now(),
			Actor:   envelope.Actor{Kind: envelope.ActorSystem, ID: "system:factory"},
			Payload: envelope.TaskStatePayload(envelope.TaskFacts{Phase: "Implementing"})},
		progress(2, "planning"),
		logEvent(3, envelope.ApprovalRequested, envelope.Actor{Kind: envelope.ActorSystem, ID: "system:policy"},
			envelope.ApprovalRequestedPayload{ApprovalID: "01M4A", CallID: "c1", Class: "forge.pr",
				Action: envelope.Must(map[string]string{"command": "push"}), ExpiresAt: time.Now().Add(time.Hour)}),
		progress(4, "checks pass"),
	}
}

type summaryBody struct {
	APIVersion string `json:"apiVersion"`
	Room       string `json:"room"`
	URL        string `json:"url"`
	Status     struct {
		Phase string `json:"phase"`
	} `json:"status"`
	NeedsYou []struct {
		ID string `json:"id"`
	} `json:"needsYou"`
	Notes struct {
		Untrusted bool `json:"untrusted"`
		Items     []struct {
			Text string `json:"text"`
		} `json:"items"`
	} `json:"notes"`
}

func summaryOf(t *testing.T, e env, room, query string, h http.Header) summaryBody {
	t.Helper()
	code, body := get(t, e, "/api/rooms/"+room+"/summary"+query, h)
	var s summaryBody
	if code != http.StatusOK || json.Unmarshal([]byte(body), &s) != nil {
		t.Fatalf("summary: %d %s", code, body)
	}
	return s
}

func TestSummaryEndpoint(t *testing.T) {
	e := setup(t, abc(t), func(s *Server, _ *fanout.Hub, _ *hubView) { s.PublicURL = "https://rooms.example" })
	factsLog(e.log)
	s := summaryOf(t, e, roomID, "", admin("boss"))
	if s.APIVersion != "summary/v1" || s.Room != roomID || s.URL != "https://rooms.example/r/"+roomID ||
		s.Status.Phase != "Implementing" || !s.Notes.Untrusted || len(s.Notes.Items) != 2 || len(s.NeedsYou) != 1 {
		t.Fatalf("%+v", s)
	}
	// A member who is not an approver is not asked to decide.
	if m := summaryOf(t, e, roomID, "", header("dev1")); len(m.NeedsYou) != 0 {
		t.Fatalf("a watcher needs %+v", m.NeedsYou)
	}
	// ?after= keeps the newer notes only.
	s = summaryOf(t, e, roomID, "?after=2", admin("boss"))
	if len(s.Notes.Items) != 1 || s.Notes.Items[0].Text != "checks pass" {
		t.Fatalf("after=2: %+v", s.Notes)
	}
	if code, _ := get(t, e, "/api/rooms/"+roomID+"/summary?after=x", admin("boss")); code != http.StatusBadRequest {
		t.Fatalf("a bad cursor: %d", code)
	}
	if code, _ := get(t, e, "/api/rooms/"+roomID+"/summary", nil); code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d", code)
	}
}

// The summary is a room read: D7 applies as it does to the WebSocket, and an unreadable room
// answers byte-for-byte like a missing one.
func TestSummaryFollowsTheGate(t *testing.T) {
	w := world(map[string][]string{"Smana/a": {"dev1"}}, "nolink")
	e := setup(t, abc(t), w.option())
	factsLog(e.log)
	_, missing := get(t, e, "/api/rooms/6kq7x2md/summary", header("dev1"))
	if missing != "no such room\n" {
		t.Fatalf("a missing room answers %q", missing)
	}
	for _, c := range []struct {
		name, room string
		h          http.Header
	}{
		{"a repository the caller cannot read", roomB, header("dev1")},
		{"a room with no repository", roomC, header("dev1")},
		{"no GitHub link", roomID, header("nolink")},
		{"a reader of nothing", roomID, header("dev2")},
	} {
		if code, body := get(t, e, "/api/rooms/"+c.room+"/summary", c.h); code != http.StatusNotFound || body != missing {
			t.Errorf("%s: %d %q, want a missing room's %q", c.name, code, body, missing)
		}
	}
	summaryOf(t, e, roomID, "", header("dev1"))
	w.set(func() { w.down, w.now = true, w.now.Add(10*time.Minute) })
	code, body := get(t, e, "/api/rooms/"+roomID+"/summary", header("dev1"))
	if code != http.StatusServiceUnavailable || body != "access_unverified\n" {
		t.Fatalf("past the cache: %d %q", code, body)
	}
}

type failingLog struct{ *memLog }

func (failingLog) Range(context.Context, string, int64, int) ([]envelope.Event, error) {
	return nil, errors.New("db down")
}

func TestSummaryLogFailureIs503(t *testing.T) {
	e := setup(t, abc(t))
	e.srv.Log = failingLog{e.log}
	code, body := get(t, e, "/api/rooms/"+roomID+"/summary", admin("boss"))
	if code != http.StatusServiceUnavailable || body != "room log unreadable\n" {
		t.Fatalf("%d %q", code, body)
	}
}

// The whole log is read, not its first page.
func TestSummaryReadsEveryPage(t *testing.T) {
	e := setup(t, abc(t))
	e.log.evs = nil
	e.log.add(1200)
	e.log.evs = append(e.log.evs, progress(1201, "last"))
	s := summaryOf(t, e, roomID, "", admin("boss"))
	if len(s.Notes.Items) != 1 || s.Notes.Items[0].Text != "last" {
		t.Fatalf("%+v", s.Notes)
	}
}

// filteredRooms serves A (Smana/a, issue by Dev1), B (Smana/b, dev1 reviews, one approval
// pending) and C (Smana/a, two approvals pending).
func filteredRooms(t *testing.T) option {
	return func(s *Server, _ *fanout.Hub, _ *hubView) {
		a := repoRoom(roomID, "Smana/a")
		a.Status.Task = &v1alpha1.TaskStatus{IssueAuthor: "Dev1"}
		b := repoRoom(roomB, "Smana/b")
		b.Status.Task = &v1alpha1.TaskStatus{PRReviewers: []string{"x", "dev1"}}
		b.Status.PendingApprovals = 1
		c := repoRoom(roomC, "Smana/a")
		c.Status.PendingApprovals = 2
		s.Rooms = roomClient(t, a, b, c)
	}
}

func TestRoomFilters(t *testing.T) {
	e := setup(t, filteredRooms(t))
	rowsOf := func(q string, h http.Header) []roomRow {
		code, body := get(t, e, "/api/rooms"+q, h)
		var rows []roomRow
		if code != http.StatusOK || json.Unmarshal([]byte(body), &rows) != nil {
			t.Fatalf("%s: %d %s", q, code, body)
		}
		return rows
	}
	want := func(q string, h http.Header, w ...string) {
		t.Helper()
		got := []string{}
		for _, r := range rowsOf(q, h) {
			got = append(got, r.ID)
		}
		slices.Sort(got)
		slices.Sort(w)
		if !slices.Equal(got, w) {
			t.Errorf("%s: %v, want %v", q, got, w)
		}
	}
	want("?repo=Smana/a", header("dev1"), roomID, roomC)
	want("?repo=Smana/b", header("dev1"), roomB)
	want("?repo=Smana/none", header("dev1"))
	// mine: the linked login, case-insensitively, as issue author or reviewer.
	want("?mine=1", header("dev1"), roomID, roomB)
	want("?mine=1", header("dev2"))
	want("?mine=1&repo=Smana/b", header("dev1"), roomB)
	// needs_me: a pending approval the caller could decide. A watcher cannot; an admin can.
	want("?needs_me=1", header("dev1"))
	want("?needs_me=1", admin("boss"), roomB, roomC)
	want("?needs_me=1&repo=Smana/a", admin("boss"), roomC)
	rows := rowsOf("?repo=Smana/b", admin("boss"))
	if len(rows) != 1 || rows[0].Repository != "Smana/b" || !rows[0].NeedsMe {
		t.Fatalf("%+v", rows)
	}
	if rows := rowsOf("?repo=Smana/b", header("dev1")); len(rows) != 1 || rows[0].NeedsMe {
		t.Fatalf("a watcher's needsMe: %+v", rows)
	}
}

// A filter never reveals a room the caller cannot read.
func TestRoomFiltersRespectTheGate(t *testing.T) {
	w := world(map[string][]string{"Smana/a": {"dev1"}})
	e := setup(t, filteredRooms(t), w.option())
	code, body := get(t, e, "/api/rooms?repo=Smana/b&mine=1", header("dev1"))
	if code != http.StatusOK || body != "[]\n" {
		t.Fatalf("%d %q", code, body)
	}
}

// mine resolves the login once per request, not once per room.
func TestMineResolvesTheLoginOnce(t *testing.T) {
	w := world(nil)
	e := setup(t, filteredRooms(t), w.option())
	before := w.links
	get(t, e, "/api/rooms?mine=1", header("dev1"))
	if w.links-before != 1 {
		t.Fatalf("%d ZITADEL link reads", w.links-before)
	}
}
