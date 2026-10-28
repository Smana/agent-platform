// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/Smana/maku/api/v1alpha1"
	"github.com/Smana/maku/internal/envelope"
	"github.com/Smana/maku/internal/fanout"
	"github.com/Smana/maku/internal/runwatch"
	"github.com/Smana/maku/internal/store"
	"github.com/Smana/maku/internal/summary"
)

// sharedStatus is the want of a named case in the status line's shared case file, the one the
// room page and maku status pass too.
func sharedStatus(t *testing.T, name string) summary.RunStatus {
	t.Helper()
	raw, err := os.ReadFile("../summary/runstatus_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name string            `json:"name"`
		Want summary.RunStatus `json:"want"`
	}
	if err := json.Unmarshal(raw, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		if c.Name == name {
			return c.Want
		}
	}
	t.Fatalf("no shared case %q", name)
	return summary.RunStatus{}
}

// Each row says the room page's status line, from the row's own data: the same function and the
// same words as the shared cases, so the list and the room page cannot disagree.
func TestRoomRowStatusIsTheStatusLine(t *testing.T) {
	now := time.Date(2026, 10, 28, 10, 4, 30, 0, time.UTC)
	live := []store.Approval{{ID: "01J9A", RoomID: roomID, Class: "shell", ExpiresAt: now.Add(time.Hour)}}
	facts := func(phase, reason string) []envelope.Event {
		return []envelope.Event{{V: 1, Seq: 1, RoomID: roomID, Type: envelope.StateChanged, TS: now,
			Payload: envelope.TaskStatePayload(envelope.TaskFacts{Phase: phase, Reason: reason})}}
	}
	running := map[string]any{"phase": "Running", "startedAt": "2026-10-28T10:00:00Z"}
	for _, tc := range []struct {
		shared    string // the shared case the row says
		room      string
		task      *v1alpha1.TaskStatus
		pending   int32
		approvals []store.Approval
		evs       []envelope.Event
		run       map[string]any // the room's run claim status, nil for none
		watcher   bool
	}{
		{shared: "running with a role and age", task: &v1alpha1.TaskStatus{Phase: "Implementing"}, run: running},
		{shared: "running between runs", task: &v1alpha1.TaskStatus{Phase: "Implementing"}},
		{shared: "pr opened, handed to the reviewer", task: &v1alpha1.TaskStatus{Phase: "Reviewing", PR: 189}},
		{shared: "queued", task: &v1alpha1.TaskStatus{Phase: "Queued"}},
		{shared: "one approval for the viewer", task: &v1alpha1.TaskStatus{Phase: "AwaitingHuman"}, pending: 1, approvals: live},
		{shared: "approvals only others decide", task: &v1alpha1.TaskStatus{Phase: "Implementing"}, pending: 2, approvals: live, watcher: true},
		{shared: "a pr to review and merge", task: &v1alpha1.TaskStatus{Phase: "AwaitingHuman", PR: 42}},
		{shared: "escalated with a failure reason", task: &v1alpha1.TaskStatus{Phase: "Escalated"}, evs: facts("Escalated", "budget-task")},
		{shared: "escalated for a maintainer", task: &v1alpha1.TaskStatus{Phase: "Escalated"}, evs: facts("Escalated", "contract_disputed")},
		{shared: "idle room", room: "Idle"},
		{shared: "done and merged", task: &v1alpha1.TaskStatus{Phase: "Done", PR: 189}},
		{shared: "done with nothing to change", task: &v1alpha1.TaskStatus{Phase: "NoOp"}},
		{shared: "failed rejected", task: &v1alpha1.TaskStatus{Phase: "Rejected"}},
		{shared: "failed with a reason", task: &v1alpha1.TaskStatus{Phase: "Rejected"}, evs: facts("Rejected", "ci_red")},
		{shared: "stopped", task: &v1alpha1.TaskStatus{Phase: "Stopped"}},
		{shared: "a sealed room without a state", room: "Sealed", task: &v1alpha1.TaskStatus{Phase: "Implementing"}},
		{shared: "an unknown state says nothing", room: "Whatever"},
	} {
		t.Run(tc.shared, func(t *testing.T) {
			e := setup(t, func(s *Server, _ *fanout.Hub, v *hubView) {
				a := repoRoom(roomID, "Smana/a")
				if tc.room != "" {
					a.Status.Phase = tc.room
				}
				a.Status.Task, a.Status.PendingApprovals = tc.task, tc.pending
				s.Rooms = roomClient(t, a)
				s.Now = func() time.Time { return now }
				v.approvals, v.evs = tc.approvals, tc.evs
				if tc.run != nil {
					runs := runwatch.New()
					runs.Upsert(t.Context(), &unstructured.Unstructured{Object: map[string]any{
						"metadata": map[string]any{"name": runwatch.ClaimPrefix() + "7f3cq2xz", "namespace": runwatch.Namespace()},
						"spec":     map[string]any{"roomRef": roomID, "role": "implementer"},
						"status":   tc.run}})
					s.Runs = runs
				}
			})
			h := admin("boss")
			if tc.watcher {
				h = header("dev1")
			}
			rows := rowsFor(t, e, "", h)
			if len(rows) != 1 {
				t.Fatalf("rows: %+v", rows)
			}
			want := sharedStatus(t, tc.shared)
			got := rows[0].Status
			if want.Kind == "" {
				if got != nil {
					t.Fatalf("an unknown state has no status, got %+v", got)
				}
				return
			}
			if got == nil || got.Kind != want.Kind || got.Icon != want.Icon || got.Text != want.Words {
				t.Fatalf("status = %+v, want %+v", got, want)
			}
		})
	}
}
