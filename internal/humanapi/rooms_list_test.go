// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"encoding/json"
	"net/http"
	"testing"

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
