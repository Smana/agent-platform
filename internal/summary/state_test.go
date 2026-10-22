// SPDX-License-Identifier: Apache-2.0

package summary

import "testing"

func TestCoarse(t *testing.T) {
	for _, c := range []struct {
		task, room string
		nextRun    bool
		want       string
	}{
		// A task's phase decides (F7's table).
		{"Received", "Open", false, StateQueued},
		{"Triaged", "Idle", false, StateQueued},
		{"Queued", "Idle", false, StateQueued},
		{"Implementing", "Active", false, StateRunning},
		{"Reviewing", "Active", false, StateRunning},
		{"AwaitingCI", "Idle", false, StateRunning},
		{"AutoMerging", "Idle", false, StateRunning},
		{"Merged", "Idle", false, StateRunning},
		{"Verifying", "Idle", false, StateRunning},
		{"AwaitingHuman", "Idle", false, StateWaiting},
		{"Escalated", "Idle", false, StateWaiting},
		{"Done", "Closed", false, StateDone},
		{"NoOp", "Closed", false, StateDone},
		{"Rejected", "Closed", false, StateFailed},
		{"Reverted", "Idle", false, StateFailed},
		{"Stopped", "Closed", false, StateStopped},
		{"Closed", "Closed", false, StateStopped},
		// A closed room stops what its task left unfinished.
		{"Implementing", "Closed", false, StateStopped},
		{"AwaitingHuman", "Closed", false, StateStopped},
		// Without a task, or with a phase this broker does not know, the room decides.
		{"", "Open", false, StateQueued},
		{"", "Active", false, StateRunning},
		{"", "Idle", true, StateRunning},
		{"", "Idle", false, StateWaiting},
		{"", "AwaitingHuman", false, StateWaiting},
		{"", "Closed", false, StateStopped},
		{"Pondering", "Active", false, StateRunning},
		{"", "", false, ""},
	} {
		if got := Coarse(c.task, c.room, c.nextRun); got != c.want {
			t.Errorf("Coarse(%q, %q, %v) = %q, want %q", c.task, c.room, c.nextRun, got, c.want)
		}
	}
}
