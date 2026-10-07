// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"fmt"

	"github.com/Smana/agent-platform/internal/envelope"
)

// StatusItem is one item of the status stream and its seq there.
type StatusItem struct {
	Seq int64
	Mapped
}

// StatusTracker turns polled execution_status values into state_changed and turn
// items (§3: state events → turn/state_changed), numbered on the status stream.
// The zero value starts the stream at seq 1; it is not safe for concurrent use.
type StatusTracker struct {
	last string
	seq  int64
	turn string
}

// Resume continues the status stream after afterSeq, the log's cursor at hello.
// Turn ids derive from these seqs, so a restarted bridge never reuses one
// (Ruling AL b).
func (t *StatusTracker) Resume(afterSeq int64) { t.seq = max(t.seq, afterSeq) }

// Emit numbers an item the bridge adds to the status stream itself.
func (t *StatusTracker) Emit(m Mapped) StatusItem {
	t.seq++
	return StatusItem{t.seq, m}
}

// Observe records one polled status. An unchanged status yields nothing; a
// change yields a harness_status item, and a turn item when it starts or ends a
// turn. waiting_for_confirmation stays inside the turn it interrupts.
func (t *StatusTracker) Observe(status, runID string) []StatusItem {
	if status == "" || status == t.last {
		return nil
	}
	prev := t.last
	t.last = status
	out := []StatusItem{t.Emit(state("harness_status", map[string]string{"status": status, "previous": prev}))}
	turn := func(phase string) StatusItem {
		it := t.Emit(Mapped{})
		// A bridge restarted mid-turn ends a turn it never saw start: it still
		// gets an id no other turn has.
		if phase == "started" || t.turn == "" {
			t.turn = fmt.Sprintf("t%d", it.Seq)
		}
		it.Mapped = Mapped{envelope.Turn, envelope.Must(envelope.TurnPayload{RunID: runID, TurnID: t.turn, Phase: phase})}
		return it
	}
	inTurn := prev == "running" || prev == "waiting_for_confirmation"
	switch {
	case status == "running" && !inTurn:
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
