// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"encoding/json"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/Smana/maku/api/v1alpha1"
	"github.com/Smana/maku/internal/envelope"
	"github.com/Smana/maku/internal/fanout"
	"github.com/Smana/maku/internal/runwatch"
	"github.com/Smana/maku/internal/summary"
)

// states is the room's coarse state as the room list and the summary give it to dev.
func states(t *testing.T, e env) (list, sum string) {
	t.Helper()
	var rows []roomRow
	if _, body := get(t, e, "/api/rooms", header("dev")); json.Unmarshal([]byte(body), &rows) != nil || len(rows) != 1 {
		t.Fatalf("rows: %s", body)
	}
	var s summary.Summary
	if _, body := get(t, e, "/api/rooms/"+roomID+"/summary", header("dev")); json.Unmarshal([]byte(body), &s) != nil {
		t.Fatalf("summary: %s", body)
	}
	return rows[0].State, s.Status.State
}

// F7: the room list and the summary say one coarse state: the task's phase, else the room's.
func TestCoarseState(t *testing.T) {
	idle := repoRoom(roomID, "Smana/a")
	idle.Status.Phase = "Idle"
	e := setup(t, func(s *Server, _ *fanout.Hub, _ *hubView) { s.Rooms = roomClient(t, idle) })
	if l, s := states(t, e); l != summary.StateWaiting || s != summary.StateWaiting {
		t.Fatalf("an idle room with no run on its way waits for its owner: list %q, summary %q", l, s)
	}

	runs := runwatch.New()
	runs.Upsert(t.Context(), &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": runwatch.ClaimPrefix() + "7f3cq2xz", "namespace": runwatch.Namespace()},
		"spec":     map[string]any{"roomRef": roomID, "role": "implementer"},
		"status":   map[string]any{"phase": "Pending"}}})
	e.srv.Runs = runs
	if l, s := states(t, e); l != summary.StateRunning || s != summary.StateRunning {
		t.Fatalf("a run on its way: list %q, summary %q", l, s)
	}

	waiting := repoRoom(roomID, "Smana/a")
	waiting.Status.Phase, waiting.Status.Task = "Idle", &v1alpha1.TaskStatus{Phase: "AwaitingHuman"}
	e = setup(t, func(s *Server, _ *fanout.Hub, _ *hubView) { s.Rooms = roomClient(t, waiting) })
	e.log.mu.Lock()
	e.log.evs = []envelope.Event{{V: 1, Seq: 1, RoomID: roomID, Type: envelope.StateChanged, TS: time.Now(),
		Actor: envelope.Actor{Kind: envelope.ActorSystem, ID: "system:factory"}, Payload: envelope.TaskStatePayload(envelope.TaskFacts{Phase: "AwaitingHuman"})}}
	e.log.mu.Unlock()
	if l, s := states(t, e); l != summary.StateWaiting || s != summary.StateWaiting {
		t.Fatalf("a task awaiting a human: list %q, summary %q", l, s)
	}

	// A sealed log stops the task it left unfinished: the summary reads the seal from the log.
	e.log.mu.Lock()
	e.log.evs = append(e.log.evs, envelope.Event{V: 1, Seq: 2, RoomID: roomID, Type: envelope.StateChanged, TS: time.Now(),
		Actor: envelope.Actor{Kind: envelope.ActorSystem, ID: "system:broker"}, Payload: envelope.StatePayload("room_phase", map[string]any{"phase": "Closed"})})
	e.log.mu.Unlock()
	if _, s := states(t, e); s != summary.StateStopped {
		t.Fatalf("a sealed room's unfinished task: summary %q", s)
	}
}
