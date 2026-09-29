// SPDX-License-Identifier: Apache-2.0

package runwatch

import (
	"context"
	"encoding/json"
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

// runScope prefixes a run's idempotency scope: broker:run:<runId>.
const runScope = "broker:run:"

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
			Origin: envelope.OriginBroker, OriginClient: runScope + r.ID, OriginSeq: step, Payload: payload})
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

// Unfinished lists the opening event of every scope that never stored its
// closing seq, in rooms still open; *store.Store has it.
type Unfinished interface {
	Unfinished(ctx context.Context, clientPrefix string, first, last int64) ([]envelope.Event, error)
}

// Sweep closes what the watch could not see: every run the log shows joined and
// never left. A run whose claim is gone was deleted while no leader watched
// (OnRemove never fired here), so it ends as deleted, with the role it joined
// with. A run still watched is observed again, which retries an append that
// failed. get is the watch's Get; call Sweep only once the watch has synced, or
// a run not yet listed would be ended. It tries every run and joins the errors.
func (e *Events) Sweep(ctx context.Context, src Unfinished, get func(runID string) (Run, bool)) error {
	open, err := src.Unfinished(ctx, runScope, stepJoined, stepLeft)
	if err != nil {
		return fmt.Errorf("runwatch: sweep: %w", err)
	}
	var errs []error
	for _, ev := range open {
		if r, ok := get(ev.RunID); ok {
			errs = append(errs, e.Observe(ctx, r))
			continue
		}
		var p envelope.ParticipantPayload
		_ = json.Unmarshal(ev.Payload, &p) // a role the log cannot give is left empty
		errs = append(errs, e.ObserveDeleted(ctx, Run{ID: ev.RunID, Room: ev.RoomID, Role: p.Role, Phase: "Pending"}))
	}
	return errors.Join(errs...)
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
