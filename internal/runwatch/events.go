// SPDX-License-Identifier: Apache-2.0

package runwatch

import (
	"context"
	"errors"
	"fmt"

	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/store"
)

// Appender is the part of the store Events writes through: broker-origin events
// take the plain Append, never the bridge's lease fence.
type Appender interface {
	Append(ctx context.Context, d envelope.Draft) (envelope.Event, bool, error)
	LastHarnessStatus(ctx context.Context, roomID, runID string) (string, error)
}

// Events writes a run's lifecycle into its room. Every step has a fixed
// idempotency key (broker:run:<runId>, step), so an informer replay or a new
// leader appends nothing twice.
type Events struct{ Store Appender }

const (
	stepJoined = iota + 1
	stepRunning
	stepEnded
	stepLeft
)

// Observe appends every lifecycle step the run has reached and the log lacks:
// joined, run_phase Running, run_phase <terminal> with its end reason, left. A
// Pending run that jumps straight to a terminal phase still records Running first,
// so the log never shows an end without a start. A run naming no room, a malformed
// one, a missing one or a sealed one appends nothing and is not an error.
func (e *Events) Observe(ctx context.Context, r Run) error {
	if !envelope.ValidID(r.Room) {
		return nil
	}
	put := func(step int64, t envelope.Type, payload []byte) error {
		_, _, err := e.Store.Append(ctx, envelope.Draft{RoomID: r.Room, RunID: r.ID,
			Actor: envelope.Actor{Kind: envelope.ActorSystem, ID: "system:room-broker"}, Type: t,
			Origin: envelope.OriginBroker, OriginClient: "broker:run:" + r.ID, OriginSeq: step, Payload: payload})
		if errors.Is(err, store.ErrNoRoom) || errors.Is(err, store.ErrSealed) {
			return nil // a run naming a missing or closed room joins nothing
		}
		if err != nil {
			return fmt.Errorf("runwatch: record step %d of run %s: %w", step, r.ID, err)
		}
		return nil
	}
	principal := "agent:" + r.ID
	if err := put(stepJoined, envelope.Participant, envelope.Must(envelope.ParticipantPayload{
		Principal: principal, Change: "joined", Role: r.Role})); err != nil {
		return err
	}
	if r.Phase == "Running" || Terminal(r.Phase) {
		if err := put(stepRunning, envelope.StateChanged, envelope.StatePayload("run_phase",
			map[string]any{"phase": "Running"})); err != nil {
			return err
		}
	}
	if !Terminal(r.Phase) {
		return nil
	}
	status, err := e.Store.LastHarnessStatus(ctx, r.Room, r.ID)
	if err != nil {
		return fmt.Errorf("runwatch: harness status of run %s: %w", r.ID, err)
	}
	if err := put(stepEnded, envelope.StateChanged, envelope.StatePayload("run_phase",
		map[string]any{"phase": r.Phase, "reason": EndReason(r, status)})); err != nil {
		return err
	}
	return put(stepLeft, envelope.Participant, envelope.Must(envelope.ParticipantPayload{
		Principal: principal, Change: "left", Role: r.Role}))
}

// ObserveDeleted ends a run whose claim was deleted before it reached a terminal
// phase (review M15): the watch then sees only the removal, never a terminal status.
// It is recorded as Revoked, reason deleted. The keys are Observe's, so a run that
// had already ended appends nothing twice.
func (e *Events) ObserveDeleted(ctx context.Context, r Run) error {
	if Terminal(r.Phase) {
		return nil
	}
	r.Phase, r.Deleted = "Revoked", true
	return e.Observe(ctx, r)
}
