// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"encoding/json"
	"net/http"
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
	three := []store.Approval{live[0], {ID: "01J9B", RoomID: roomID, Class: "shell", ExpiresAt: now.Add(time.Hour)},
		{ID: "01J9C", RoomID: roomID, Class: "shell", ExpiresAt: now.Add(time.Hour)}}
	answer := []envelope.Event{{V: 1, Seq: 1, RoomID: roomID, Type: envelope.StateChanged, TS: now,
		Payload: envelope.TaskStatePayload(envelope.TaskFacts{Phase: "Escalated", Reason: "needs_author",
			Issue: &envelope.IssueFact{Number: 7, URL: "https://github.com/Smana/a/issues/7", Author: "alice"}})}}
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
		{shared: "pr opened, waiting for ci", task: &v1alpha1.TaskStatus{Phase: "AwaitingCI", PR: 189}},
		{shared: "several approvals for the viewer", task: &v1alpha1.TaskStatus{Phase: "Implementing"}, pending: 3, approvals: three},
		{shared: "an issue to answer", task: &v1alpha1.TaskStatus{Phase: "Escalated", Issue: 7}, evs: answer},
		{shared: "pr opened, handed to the reviewer", task: &v1alpha1.TaskStatus{Phase: "Reviewing", PR: 189}},
		{shared: "queued", task: &v1alpha1.TaskStatus{Phase: "Queued"}},
		{shared: "one approval for the viewer", task: &v1alpha1.TaskStatus{Phase: "AwaitingHuman"}, pending: 1, approvals: live},
		{shared: "approvals only others decide", task: &v1alpha1.TaskStatus{Phase: "Implementing"}, pending: 2, approvals: live, watcher: true},
		{shared: "a pr to review and merge", task: &v1alpha1.TaskStatus{Phase: "AwaitingHuman", PR: 42}},
		{shared: "escalated with a failure reason still waits", task: &v1alpha1.TaskStatus{Phase: "Escalated"}, evs: facts("Escalated", "budget-task")},
		{shared: "escalated for a maintainer", task: &v1alpha1.TaskStatus{Phase: "Escalated"}, evs: facts("Escalated", "contract_disputed")},
		{shared: "idle room", room: "Idle"},
		{shared: "done and merged", task: &v1alpha1.TaskStatus{Phase: "Done", PR: 189}},
		{shared: "done with nothing to change", task: &v1alpha1.TaskStatus{Phase: "NoOp"}},
		{shared: "failed rejected", task: &v1alpha1.TaskStatus{Phase: "Rejected"}},
		{shared: "failed on the budget", task: &v1alpha1.TaskStatus{Phase: "Rejected"}, evs: facts("Rejected", "budget-task")},
		{shared: "failed reverted", task: &v1alpha1.TaskStatus{Phase: "Reverted"}},
		{shared: "failed with a reason says why", task: &v1alpha1.TaskStatus{Phase: "Rejected"}, evs: facts("Rejected", "ci_red")},
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

// One room, read through the list and through its summary: the row's status and summary.StatusOf
// of the summary say the same line, for the cases where they could drift: a failed task with a
// reason code, an escalated one with a failure code, a reverted merge, an approval its viewer
// prompted under four-eyes, and a watcher who cannot decide.
func TestRoomRowStatusIsTheSummarysStatus(t *testing.T) {
	now := time.Date(2026, 10, 28, 10, 4, 30, 0, time.UTC)
	fact := func(seq int64, phase, reason string) envelope.Event {
		return envelope.Event{V: 1, Seq: seq, RoomID: roomID, Type: envelope.StateChanged, TS: now,
			Actor: envelope.Actor{Kind: envelope.ActorSystem, ID: "system:factory"}, Payload: envelope.TaskStatePayload(envelope.TaskFacts{Phase: phase, Reason: reason})}
	}
	asked := func(seq int64, prompters ...string) (envelope.Event, store.Approval) {
		p, _ := json.Marshal(envelope.ApprovalRequestedPayload{ApprovalID: "01M4A", CallID: "c1", Class: "shell", ExpiresAt: now.Add(time.Hour), Prompters: prompters})
		return envelope.Event{V: 1, Seq: seq, RoomID: roomID, Type: envelope.ApprovalRequested, TS: now,
				Actor: envelope.Actor{Kind: envelope.ActorSystem, ID: "system:policy"}, Payload: p},
			store.Approval{ID: "01M4A", RoomID: roomID, Class: "shell", State: "pending", Prompters: prompters, ExpiresAt: now.Add(time.Hour)}
	}
	ask, approval := asked(2)
	ask4, approval4 := asked(2, "human:boss")
	for _, tc := range []struct {
		name      string
		phase     string
		evs       []envelope.Event
		approvals []store.Approval
		fourEyes  bool
		h         http.Header
		want      string
	}{
		{name: "rejected for ci_red", phase: "Rejected", evs: []envelope.Event{fact(1, "Rejected", "ci_red")}, h: admin("boss"), want: "Failed — CI stayed red"},
		{name: "rejected on the budget", phase: "Rejected", evs: []envelope.Event{fact(1, "Rejected", "budget-task")}, h: admin("boss"), want: "Failed — budget reached"},
		{name: "escalated for budget-task", phase: "Escalated", evs: []envelope.Event{fact(1, "Escalated", "budget-task")}, h: admin("boss"), want: "Waiting — a maintainer decides"},
		{name: "reverted", phase: "Reverted", evs: []envelope.Event{fact(1, "Reverted", "")}, h: admin("boss"), want: "Failed — the merge was reverted"},
		{name: "an approval the viewer may decide", phase: "Implementing", evs: []envelope.Event{fact(1, "Implementing", ""), ask},
			approvals: []store.Approval{approval}, h: admin("boss"), want: "Waiting for you — approve a pending action"},
		{name: "an approval the viewer prompted, under four-eyes", phase: "Implementing", evs: []envelope.Event{fact(1, "Implementing", ""), ask4},
			approvals: []store.Approval{approval4}, fourEyes: true, h: admin("boss"), want: "Waiting for another approver"},
		{name: "a watcher", phase: "Implementing", evs: []envelope.Event{fact(1, "Implementing", ""), ask},
			approvals: []store.Approval{approval}, h: header("dev1"), want: "Waiting for another approver"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t, func(s *Server, _ *fanout.Hub, v *hubView) {
				a := repoRoom(roomID, "Smana/a")
				a.Status.Task = &v1alpha1.TaskStatus{Phase: tc.phase}
				a.Status.PendingApprovals = int32(len(tc.approvals))
				a.Spec.Approvals.FourEyes = tc.fourEyes
				s.Rooms = roomClient(t, a)
				s.Now = func() time.Time { return now }
				v.approvals, v.evs = tc.approvals, tc.evs
			})
			rows := rowsFor(t, e, "", tc.h)
			if len(rows) != 1 || rows[0].Status == nil {
				t.Fatalf("rows: %+v", rows)
			}
			code, body := get(t, e, "/api/rooms/"+roomID+"/summary", tc.h)
			var sum summary.Summary
			if code != http.StatusOK || json.Unmarshal([]byte(body), &sum) != nil {
				t.Fatalf("summary: %d %s", code, body)
			}
			page := summary.StatusOf(sum, now)
			if got := rows[0].Status; got.Kind != page.Kind || got.Icon != page.Icon || got.Text != page.Words {
				t.Fatalf("the list says %+v, the summary %+v", got, page)
			}
			if page.Words != tc.want {
				t.Fatalf("both say %q, want %q", page.Words, tc.want)
			}
		})
	}
}

// The failed rows' reason codes come from one store read for the whole list, never one per row.
func TestRoomRowsReadFailureCodesInOneCall(t *testing.T) {
	var log *memLog
	e := setup(t, func(s *Server, _ *fanout.Hub, v *hubView) {
		log = v.memLog
		a, b, c := repoRoom(roomID, "Smana/a"), repoRoom(roomB, "Smana/a"), repoRoom(roomC, "Smana/a")
		a.Status.Task = &v1alpha1.TaskStatus{Phase: "Rejected"}
		b.Status.Task = &v1alpha1.TaskStatus{Phase: "Reverted"}
		c.Status.Task = &v1alpha1.TaskStatus{Phase: "Implementing"}
		s.Rooms = roomClient(t, a, b, c)
		v.evs = []envelope.Event{{V: 1, Seq: 1, RoomID: roomID, Type: envelope.StateChanged, TS: time.Now(),
			Payload: envelope.TaskStatePayload(envelope.TaskFacts{Phase: "Rejected", Reason: "ci_red"})}}
	})
	rows := rowsFor(t, e, "", admin("boss"))
	words := map[string]string{}
	for _, r := range rows {
		if r.Status != nil {
			words[r.ID] = r.Status.Text
		}
	}
	if words[roomID] != "Failed — CI stayed red" || words[roomB] != "Failed — the merge was reverted" {
		t.Fatalf("statuses: %v", words)
	}
	if log.reasonReads != 1 || log.factsReads != 0 {
		t.Fatalf("%d batch reads and %d per-room reads, want one batch and none per room", log.reasonReads, log.factsReads)
	}
}
