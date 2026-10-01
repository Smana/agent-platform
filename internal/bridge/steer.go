// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"context"
	"sync"

	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/wire"
)

// Steering injects the driver's messages and interrupts into the harness, once
// each, and acknowledges them on the status stream (§2 Steering, Interrupt).
//
// A failed injection returns its error, which ends the broker stream: the
// re-dialled stream replays from the log's last acknowledgement, so the failed
// ref is retried before any later one is handed over. Acknowledging a later ref
// first would move LastAck past the failed one for good.
type Steering struct {
	Harness *Harness
	RunID   string
	Push    func(wire.Item)

	mu      sync.Mutex
	handled map[int64]bool
}

// once claims ref; false when it was handled already, a replayed frame.
func (s *Steering) once(ref int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.handled == nil {
		s.handled = map[int64]bool{}
	}
	if s.handled[ref] {
		return false
	}
	s.handled[ref] = true
	return true
}

func (s *Steering) forget(ref int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.handled, ref)
}

func (s *Steering) ack(kind string, ref int64) {
	s.Push(wire.Item{Stream: wire.StreamStatus, Type: envelope.StateChanged,
		Payload: envelope.StatePayload(kind, map[string]any{"ref": ref, "runId": s.RunID})})
}

// Deliver sends a steering message, consumed by OpenHands at its next step (run: true).
func (s *Steering) Deliver(ctx context.Context, d wire.Deliver) error {
	if !s.once(d.Ref) {
		return nil
	}
	if err := s.Harness.Send(ctx, d.Text); err != nil {
		s.forget(d.Ref)
		return err
	}
	s.ack("delivered", d.Ref)
	return nil
}

// Interrupt stops the run's current turn.
func (s *Steering) Interrupt(ctx context.Context, i wire.Interrupt) error {
	if !s.once(i.Ref) {
		return nil
	}
	if err := s.Harness.Interrupt(ctx); err != nil {
		s.forget(i.Ref)
		return err
	}
	s.ack("interrupted", i.Ref)
	return nil
}
