// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/fanout"
	"github.com/Smana/agent-platform/internal/store"
	"github.com/Smana/agent-platform/internal/summary"
)

// #50: the list carries what a person needs to tell the rows apart — the issue, its title, the
// task's phase and its PR — and #51: a PR waiting on the merge counts as a need for whoever owns it.
func TestRoomListCarriesTheTask(t *testing.T) {
	e := setup(t, func(s *Server, _ *fanout.Hub, _ *hubView) {
		a := repoRoom(roomID, "Smana/a")
		a.Status.Phase = "Idle"
		a.Status.PendingApprovals = 1
		a.Status.Task = &v1alpha1.TaskStatus{Phase: "AwaitingHuman", Issue: 2266, IssueTitle: "docs(agents): runbook 11", PR: 2267}
		s.Rooms = roomClient(t, a, repoRoom(roomB, "Smana/a"))
	})
	rowsOf := func(q string, h http.Header) map[string]roomRow {
		t.Helper()
		code, body := get(t, e, "/api/rooms"+q, h)
		var rows []roomRow
		if code != http.StatusOK || json.Unmarshal([]byte(body), &rows) != nil {
			t.Fatalf("%d %s", code, body)
		}
		out := map[string]roomRow{}
		for _, r := range rows {
			out[r.ID] = r
		}
		return out
	}
	r := rowsOf("", admin("boss"))[roomID]
	if r.Issue != 2266 || r.Title != "docs(agents): runbook 11" || r.Task != "AwaitingHuman" || r.PR != 2267 ||
		r.Phase != "Idle" || r.NeedsYou != 2 || !r.NeedsMe {
		t.Fatalf("an owner's row: %+v", r)
	}
	if bare := rowsOf("", admin("boss"))[roomB]; bare.Issue != 0 || bare.Task != "" || bare.PR != 0 || bare.NeedsYou != 0 {
		t.Fatalf("a room with no task: %+v", bare)
	}
	if w := rowsOf("", header("dev1"))[roomID]; w.NeedsYou != 0 || w.NeedsMe || w.Issue != 2266 {
		t.Fatalf("a watcher cannot merge or decide, but sees the task: %+v", w)
	}
	if got := rowsOf("?needs_me=1", admin("boss")); len(got) != 1 || got[roomID].ID == "" {
		t.Fatalf("needs_me: %+v", got)
	}
}

// The list says when each room's log last moved; a failing read leaves the field out and the list intact.
func TestRoomListCarriesLastEventAt(t *testing.T) {
	at := time.Date(2026, 10, 15, 10, 0, 0, 0, time.UTC)
	var log *memLog
	e := setup(t, func(s *Server, _ *fanout.Hub, v *hubView) {
		log = v.memLog
		s.Rooms = roomClient(t, repoRoom(roomID, "Smana/a"))
	})
	rows := func() []roomRow {
		t.Helper()
		code, body := get(t, e, "/api/rooms", admin("boss"))
		var rows []roomRow
		if code != http.StatusOK || json.Unmarshal([]byte(body), &rows) != nil || len(rows) != 1 {
			t.Fatalf("%d %s", code, body)
		}
		return rows
	}
	log.lastEventAt = map[string]time.Time{roomID: at}
	if got := rows()[0].LastEventAt; !got.Equal(at) {
		t.Fatalf("lastEventAt = %v, want %v", got, at)
	}
	log.lastEventAtErr = errors.New("db down")
	if got := rows()[0].LastEventAt; !got.IsZero() {
		t.Fatalf("a failed read must leave lastEventAt zero, got %v", got)
	}
}

// The row names the first need of the caller, in the summary's order: approvals (lowest id), then
// a hold, then a merge. Nothing that waits for the caller means no `need` key.
func TestRoomRowNeedIsTheFirstNeed(t *testing.T) {
	future, past := time.Now().Add(time.Hour).UTC().Truncate(time.Second), time.Now().Add(-time.Hour).UTC()
	approval := func(id, cmd string, exp time.Time) store.Approval {
		return store.Approval{ID: id, RoomID: roomID, Class: "shell", Action: json.RawMessage(`{"command":"` + cmd + `"}`), State: "pending", ExpiresAt: exp}
	}
	for _, tc := range []struct {
		name      string
		task      *v1alpha1.TaskStatus
		pending   int32
		approvals []store.Approval
		facts     *envelope.TaskFacts
		h         http.Header
		want      *summary.Need
	}{
		{name: "two approvals: the lowest id", pending: 2, h: admin("boss"),
			approvals: []store.Approval{approval("01J9B", "rm -rf b", future), approval("01J9A", "make test", future)},
			want:      &summary.Need{Kind: "approval", ID: "01J9A", What: "make test", Deadline: future, URL: "https://rooms.test/r/" + roomID + "#01J9A"}},
		{name: "an expired approval gives way to the hold", pending: 1, h: admin("boss"),
			task:      &v1alpha1.TaskStatus{Phase: "Queued"},
			approvals: []store.Approval{approval("01J9A", "make test", past)},
			facts:     &envelope.TaskFacts{Phase: "Queued", Reason: "waiting_human_driver"},
			want:      &summary.Need{Kind: "hold", ID: "waiting_human_driver", What: "", URL: "https://rooms.test/r/" + roomID}},
		{name: "held for the caller", h: admin("boss"),
			task:  &v1alpha1.TaskStatus{Phase: "Queued"},
			facts: &envelope.TaskFacts{Phase: "Queued", Reason: "waiting_human_driver"},
			want:  &summary.Need{Kind: "hold", ID: "waiting_human_driver", URL: "https://rooms.test/r/" + roomID}},
		{name: "a PR waits for the caller's merge", h: admin("boss"),
			task: &v1alpha1.TaskStatus{Phase: "AwaitingHuman", PR: 7},
			want: &summary.Need{Kind: "merge", ID: "7", URL: "https://github.com/Smana/a/pull/7"}},
		{name: "an approval the caller may not decide", pending: 1, h: header("dev1"),
			approvals: []store.Approval{approval("01J9A", "make test", future)}},
		{name: "nothing waits", h: admin("boss")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t, func(s *Server, _ *fanout.Hub, v *hubView) {
				a := repoRoom(roomID, "Smana/a")
				a.Status.PendingApprovals, a.Status.Task = tc.pending, tc.task
				s.Rooms = roomClient(t, a)
				s.PublicURL = "https://rooms.test"
				v.approvals = tc.approvals
				if tc.facts != nil {
					v.evs = append(v.evs, envelope.Event{V: 1, Seq: int64(len(v.evs) + 1), RoomID: roomID, Type: envelope.StateChanged,
						TS: time.Now(), Payload: envelope.TaskStatePayload(*tc.facts)})
				}
			})
			code, body := get(t, e, "/api/rooms", tc.h)
			var rows []roomRow
			if code != http.StatusOK || json.Unmarshal([]byte(body), &rows) != nil || len(rows) != 1 {
				t.Fatalf("%d %s", code, body)
			}
			got := rows[0].Need
			switch {
			case tc.want == nil:
				if got != nil || strings.Contains(body, `"need"`) {
					t.Fatalf("no need expected: %s", body)
				}
			case got == nil:
				t.Fatalf("need missing: %s", body)
			default:
				w := *tc.want
				if w.Kind == "hold" || w.Kind == "merge" {
					w.What = got.What // the wording is the summary's, tested there
					if got.What == "" {
						t.Fatalf("a %s need says nothing: %+v", w.Kind, got)
					}
				}
				if got.Kind != w.Kind || got.ID != w.ID || got.What != w.What || got.URL != w.URL || !got.Deadline.Equal(w.Deadline) {
					t.Fatalf("need = %+v, want %+v", *got, w)
				}
			}
		})
	}
}

// A room row reads the approvals table only for a room that has one pending.
func TestRoomRowsReadApprovalsOnlyWhenPending(t *testing.T) {
	var log *memLog
	e := setup(t, func(s *Server, _ *fanout.Hub, v *hubView) {
		log = v.memLog
		a := repoRoom(roomID, "Smana/a")
		a.Status.PendingApprovals = 1
		s.Rooms = roomClient(t, a, repoRoom(roomB, "Smana/a"), repoRoom(roomC, "Smana/a"))
		log.approvals = []store.Approval{{ID: "01J9A", RoomID: roomID, Class: "shell", ExpiresAt: time.Now().Add(time.Hour)}}
	})
	if code, body := get(t, e, "/api/rooms", admin("boss")); code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
	if log.approvalReads != 1 {
		t.Fatalf("OpenApprovals called %d times for 3 rooms, one pending, want 1", log.approvalReads)
	}
}
