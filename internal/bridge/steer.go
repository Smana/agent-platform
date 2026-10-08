// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/wire"
)

// Steering injects the driver's messages and interrupts into the harness, once
// each, and acknowledges them on the status stream (§2 Steering, Interrupt).
//
// A transient failure returns its error, which ends the broker stream: the
// re-dialled stream replays from the log's last acknowledgement, so the failed
// ref is retried before any later one is handed over. Acknowledging a later ref
// first would move LastAck past the failed one for good. A permanent refusal
// (review 4.3 I2; for an interrupt, any 4xx) is acknowledged as undeliverable
// instead, with the harness's status code: retrying it would block every later ref.
// Delivery is at least once: a ref injected but not yet acknowledged in the log
// when the bridge restarts is injected again.
type Steering struct {
	Harness *Harness
	RunID   string
	Push    func(wire.Item)
	// Gate runs each steering send (Confirmer.Gate), deciding its run flag: a message with run:true
	// would confirm a waiting step implicitly, so the step is rejected first.
	Gate func(ctx context.Context, send func(run bool) error) error

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

func (s *Steering) ack(kind string, ref int64, code int) {
	fields := map[string]any{"ref": ref, "runId": s.RunID}
	if code != 0 {
		fields["code"] = code
	}
	s.Push(wire.Item{Stream: wire.StreamStatus, Type: envelope.StateChanged, Payload: envelope.StatePayload(kind, fields)})
}

// refused reports the harness's status code for a 4xx refusal.
func refused(err error) (int, bool) {
	se, ok := errors.AsType[*StatusError](err)
	if !ok || se.Code < 400 || se.Code >= 500 {
		return 0, false
	}
	return se.Code, true
}

// permanent reports the code of a steering refusal no retry can fix: a 4xx
// other than those that mean "not now" (a conversation not created yet, a
// timeout, a conflict, too early or too many).
func permanent(err error) (int, bool) {
	code, ok := refused(err)
	switch code {
	case http.StatusNotFound, http.StatusRequestTimeout, http.StatusConflict, http.StatusTooEarly, http.StatusTooManyRequests:
		return 0, false
	}
	return code, ok
}

// failed settles a failed injection of ref: acknowledged as undeliverable when
// settle says so, else released for the replay, and its error ends the stream.
func (s *Steering) failed(ref int64, err error, settle func(error) (int, bool)) error {
	if code, ok := settle(err); ok {
		s.ack("undeliverable", ref, code)
		return nil
	}
	s.forget(ref)
	return err
}

// Deliver sends a steering message, consumed by OpenHands at its next step (run: true).
func (s *Steering) Deliver(ctx context.Context, d wire.Deliver) error {
	if !s.once(d.Ref) {
		return nil
	}
	send := func(run bool) error { return s.Harness.Message(ctx, d.Text, run) }
	var err error
	if s.Gate != nil {
		err = s.Gate(ctx, send)
	} else {
		err = send(true) // phase 4, with no confirmation loop
	}
	if err != nil {
		return s.failed(d.Ref, err, permanent)
	}
	s.ack("delivered", d.Ref, 0)
	return nil
}

// Interrupt stops the run's current turn. Any 4xx settles it as undeliverable
// (ruling SAK): an interrupt of an idle conversation is moot, a late one lands
// on a turn the driver never meant, and retrying it would starve the steering
// queued behind it.
func (s *Steering) Interrupt(ctx context.Context, i wire.Interrupt) error {
	if !s.once(i.Ref) {
		return nil
	}
	if err := s.Harness.Interrupt(ctx); err != nil {
		return s.failed(i.Ref, err, refused)
	}
	s.ack("interrupted", i.Ref, 0)
	return nil
}
