// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/fanout"
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
