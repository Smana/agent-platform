// SPDX-License-Identifier: Apache-2.0

package summary

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func TestCoarse(t *testing.T) {
	for _, c := range []struct {
		task, room string
		nextRun    bool
		want       string
	}{
		// A task's phase decides (F7's table).
		{"Received", "Open", false, StateQueued},
		{"Split", "Idle", false, StateRunning},
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
		{"", "", false, StateQueued},
		{"", "Pondering", false, ""},
	} {
		if got := Coarse(c.task, c.room, c.nextRun); got != c.want {
			t.Errorf("Coarse(%q, %q, %v) = %q, want %q", c.task, c.room, c.nextRun, got, c.want)
		}
	}
}

func TestTaskPhasesCoverCoarse(t *testing.T) {
	phases := TaskPhases()
	if len(phases) == 0 || !slices.IsSorted(phases) {
		t.Fatalf("phases %v: want a non-empty sorted list", phases)
	}
	for _, p := range phases {
		if Coarse(p, "Open", false) == "" {
			t.Errorf("phase %q has no coarse state", p)
		}
	}
}

func TestIsHold(t *testing.T) {
	for reason, want := range map[string]bool{"waiting_human_driver": true, "waiting_room_busy": true, "waiting_run_slot": false, "": false} {
		if got := IsHold(reason); got != want {
			t.Errorf("IsHold(%q) = %v, want %v", reason, got, want)
		}
	}
}

func TestDescribeApprovalMatchesTheNeed(t *testing.T) {
	long := `{"command":"` + strings.Repeat("x", 300) + `"}`
	for name, tc := range map[string]struct{ class, action, want string }{
		"command":   {"forge.pr", `{"command":"git push"}`, "git push"},
		"class":     {"forge.pr", `{}`, "forge.pr"},
		"not json":  {"shell", `nope`, "shell"},
		"truncated": {"shell", long, strings.Repeat("x", 200) + "…"},
	} {
		if got := DescribeApproval(tc.class, json.RawMessage(tc.action)); got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}
