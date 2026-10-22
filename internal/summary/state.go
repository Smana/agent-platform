// SPDX-License-Identifier: Apache-2.0

package summary

// The coarse states (factory interfaces F7): one vocabulary for summary/v1, the room list, the MCP
// server and the later webhooks. Each maps onto one A2A task state (docs/api.md, The coarse state).
const (
	StateQueued  = "queued"
	StateRunning = "running"
	StateWaiting = "waiting_for_human"
	StateDone    = "done"
	StateFailed  = "failed"
	StateStopped = "stopped"
)

// taskStates maps the Task CRD's phases (api/factory/v1alpha1) to coarse states, repeated here
// like stopLabel: this package stays free of the factory's imports. Merged runs on: the factory
// verifies the merge before Done.
func taskStates() map[string]string {
	return map[string]string{
		"Received": StateQueued, "Triaged": StateQueued, "Queued": StateQueued,
		"Implementing": StateRunning, "Reviewing": StateRunning, "AwaitingCI": StateRunning,
		"AutoMerging": StateRunning, "Merged": StateRunning, "Verifying": StateRunning,
		"AwaitingHuman": StateWaiting, "Escalated": StateWaiting,
		"Done": StateDone, "NoOp": StateDone,
		"Rejected": StateFailed, "Reverted": StateFailed,
		"Stopped": StateStopped, "Closed": StateStopped,
	}
}

// Coarse is a room's coarse state. task is its task's phase, "" when no Task owns the room; room
// is the Room's projected phase (Open, Active, Idle, AwaitingHuman or Closed); nextRun is whether a
// live run in it has not started running yet. A known task phase decides, except in a closed room,
// which stops a task it left unfinished; otherwise the room's phase decides. An idle room waits for
// its owner unless a run is on its way. A room with no phase yet and no task has not started: queued.
// "" when the room phase is one this broker does not know (omitted, never a guess).
func Coarse(task, room string, nextRun bool) string {
	if s, ok := taskStates()[task]; ok {
		if room == "Closed" && s != StateDone && s != StateFailed && s != StateStopped {
			return StateStopped
		}
		return s
	}
	switch room {
	case "Open", "":
		return StateQueued
	case "Active":
		return StateRunning
	case "Idle":
		if nextRun {
			return StateRunning
		}
		return StateWaiting
	case "AwaitingHuman":
		return StateWaiting
	case "Closed":
		return StateStopped
	}
	return ""
}
