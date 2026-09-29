// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"fmt"

	"github.com/Smana/agent-platform/internal/envelope"
)

// StatusTracker turns polled execution_status values into state_changed and turn
// items (§3: state events → turn/state_changed). Its items go on the status
// stream. The zero value is ready; it is not safe for concurrent use.
type StatusTracker struct {
	last string
	turn int
}

// Observe records one polled status. An unchanged status yields nothing; a
// change yields a harness_status item, and a turn item when it starts or ends a
// turn. waiting_for_confirmation stays inside the turn it interrupts.
func (t *StatusTracker) Observe(status, runID string) []Mapped {
	if status == "" || status == t.last {
		return nil
	}
	prev := t.last
	t.last = status
	out := []Mapped{state("harness_status", map[string]string{"status": status, "previous": prev})}
	turn := func(phase string) Mapped {
		return Mapped{envelope.Turn, envelope.Must(envelope.TurnPayload{RunID: runID,
			TurnID: fmt.Sprintf("t%d", t.turn), Phase: phase})}
	}
	inTurn := prev == "running" || prev == "waiting_for_confirmation"
	switch {
	case status == "running" && !inTurn:
		t.turn++
		out = append(out, turn("started"))
	case inTurn:
		switch status {
		case "finished", "idle":
			out = append(out, turn("completed"))
		case "paused":
			out = append(out, turn("cancelled"))
		case "error", "stuck":
			out = append(out, turn("failed"))
		}
	}
	return out
}
