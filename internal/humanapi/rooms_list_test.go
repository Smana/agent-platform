// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Smana/maku/api/v1alpha1"
	"github.com/Smana/maku/internal/envelope"
	"github.com/Smana/maku/internal/fanout"
	"github.com/Smana/maku/internal/store"
	"github.com/Smana/maku/internal/summary"
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

func rowsFor(t *testing.T, e env, q string, h http.Header) []roomRow {
	t.Helper()
	code, body := get(t, e, "/api/rooms"+q, h)
	var rows []roomRow
	if code != http.StatusOK || json.Unmarshal([]byte(body), &rows) != nil {
		t.Fatalf("%d %s", code, body)
	}
	return rows
}

// The answer need counts in a row as in the card: the owner of an Escalated room waiting for the
// issue's author needs to answer, so the row, needs_me and the inbox agree. One facts read serves
// the count, the reason and the need.
func TestRoomRowCountsTheAnswerNeed(t *testing.T) {
	var log *memLog
	e := setup(t, func(s *Server, _ *fanout.Hub, v *hubView) {
		log = v.memLog
		a := repoRoom(roomID, "Smana/a")
		a.Status.Task = &v1alpha1.TaskStatus{Phase: "Escalated", Issue: 42}
		s.Rooms = roomClient(t, a)
		s.PublicURL = "https://rooms.test"
		f := envelope.TaskFacts{Phase: "Escalated", Reason: "needs_author", Issue: &envelope.IssueFact{Number: 42, URL: "https://github.com/Smana/a/issues/42", Author: "alice"}}
		v.evs = append(v.evs, envelope.Event{V: 1, Seq: 1, RoomID: roomID, Type: envelope.StateChanged, TS: time.Now(), Payload: envelope.TaskStatePayload(f)})
	})
	rows := rowsFor(t, e, "?needs_me=1", admin("boss"))
	if len(rows) != 1 || rows[0].NeedsYou != 1 || !rows[0].NeedsMe || rows[0].Need == nil || rows[0].Need.Kind != "answer" || rows[0].Need.ID != "42" {
		t.Fatalf("an owner's row: %+v", rows)
	}
	if rows[0].Reason != "needs_author" || log.factsReads != 1 {
		t.Errorf("reason %q after %d facts reads, want one read", rows[0].Reason, log.factsReads)
	}
	if st := rows[0].Step; st == nil || st.Kind != summary.StepAnswer || st.Label != "answer the question on issue #42" || st.URL != "https://github.com/Smana/a/issues/42" {
		t.Errorf("an owner's step: %+v", st)
	}
	if w := rowsFor(t, e, "", header("dev1")); len(w) != 1 || w[0].NeedsYou != 0 || w[0].Need != nil || w[0].Step != nil {
		t.Errorf("a watcher has no need or step: %+v", w)
	}
}

// An approval past its deadline that the sweep has not decided yet is no need: the count and the
// first need agree with the summary, which drops it.
func TestRoomRowSkipsAnUnsweptExpiredApproval(t *testing.T) {
	e := setup(t, func(s *Server, _ *fanout.Hub, v *hubView) {
		a := repoRoom(roomID, "Smana/a")
		a.Status.PendingApprovals = 2
		s.Rooms = roomClient(t, a)
		v.approvals = []store.Approval{
			{ID: "01J9A", RoomID: roomID, Class: "shell", ExpiresAt: time.Now().Add(-time.Hour)},
			{ID: "01J9B", RoomID: roomID, Class: "shell", ExpiresAt: time.Now().Add(time.Hour)},
		}
	})
	rows := rowsFor(t, e, "", admin("boss"))
	if len(rows) != 1 || rows[0].NeedsYou != 1 || rows[0].Need == nil || rows[0].Need.ID != "01J9B" {
		t.Fatalf("one live approval: %+v", rows)
	}
	e2 := setup(t, func(s *Server, _ *fanout.Hub, v *hubView) {
		a := repoRoom(roomID, "Smana/a")
		a.Status.PendingApprovals = 1
		s.Rooms = roomClient(t, a)
		v.approvals = []store.Approval{{ID: "01J9A", RoomID: roomID, Class: "shell", ExpiresAt: time.Now().Add(-time.Hour)}}
	})
	if rows := rowsFor(t, e2, "?needs_me=1", admin("boss")); len(rows) != 0 {
		t.Fatalf("only an expired approval: needs_me lists %+v", rows)
	}
	if rows := rowsFor(t, e2, "", admin("boss")); len(rows) != 1 || rows[0].NeedsYou != 0 || rows[0].NeedsMe || rows[0].Need != nil {
		t.Fatalf("the row says it needs you with nothing to show: %+v", rows)
	}
}

// Every row names the one step for its room: approve on the page, or merge or close the PR on
// GitHub, for a caller who may not close it too. The MCP rows are these rows.
func TestRoomRowStep(t *testing.T) {
	future := time.Now().Add(time.Hour).UTC()
	approval := store.Approval{ID: "01J9A", RoomID: roomID, Class: "shell", Action: json.RawMessage(`{"command":"make"}`), State: "pending", ExpiresAt: future}
	pr := &v1alpha1.TaskStatus{Phase: "AwaitingHuman", PR: 7}
	for _, tc := range []struct {
		name      string
		task      *v1alpha1.TaskStatus
		pending   int32
		phase     string
		approvals []store.Approval
		h         http.Header
		want      *summary.Step
	}{
		{name: "a PR held for an owner", task: pr, h: admin("boss"),
			want: &summary.Step{Kind: summary.StepPR, Label: "review and merge or close PR #7", URL: "https://github.com/Smana/a/pull/7"}},
		{name: "a PR held, a watcher cannot close it", task: pr, h: header("dev1")},
		{name: "a sealed room", task: pr, phase: "Sealed", h: admin("boss")},
		{name: "an approval wins", task: pr, pending: 1, approvals: []store.Approval{approval}, h: admin("boss"),
			want: &summary.Step{Kind: summary.StepApprove, Label: "approve here", URL: "https://rooms.test/r/" + roomID}},
		{name: "a watcher cannot decide", pending: 1, approvals: []store.Approval{approval}, h: header("dev1")},
		{name: "a running task", task: &v1alpha1.TaskStatus{Phase: "Implementing", PR: 7}, h: admin("boss")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t, func(s *Server, _ *fanout.Hub, v *hubView) {
				a := repoRoom(roomID, "Smana/a")
				a.Status.PendingApprovals, a.Status.Task = tc.pending, tc.task
				if tc.phase != "" {
					a.Status.Phase = tc.phase
				}
				s.Rooms = roomClient(t, a)
				s.PublicURL = "https://rooms.test"
				v.approvals = tc.approvals
			})
			rows := rowsFor(t, e, "", tc.h)
			if len(rows) != 1 {
				t.Fatalf("rows: %+v", rows)
			}
			if got := rows[0].Step; !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("step = %+v, want %+v", got, tc.want)
			}
		})
	}
}
