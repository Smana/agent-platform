// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/Smana/agent-platform/internal/envelope"
)

var (
	// ErrStaleEpoch is a driver change whose expected epoch the room has moved past.
	ErrStaleEpoch = errors.New("stale_epoch")
	// ErrInvalidDriver is a change to no principal, or to the current holder.
	ErrInvalidDriver = errors.New("the next driver is a principal other than the holder")
)

// principalKinds are the prefixes a driver can carry.
var principalKinds = []string{"human:", "system:", "agent:"}

// ChangeDriver moves the token only if the room's epoch is still expect, the
// fence that makes a give, a take and a lease expiry safe across replicas (§2).
// d carries the actor, origin and idempotency key; the store fills the type and
// the DriverPayload. A replayed key returns the stored event and moves nothing.
func (s *Store) ChangeDriver(ctx context.Context, roomID string, expect int64, to, reason string, d envelope.Draft) (envelope.Event, error) {
	ev, err := s.changeDriver(ctx, roomID, expect, to, reason, d)
	if err != nil {
		return envelope.Event{}, fmt.Errorf("store: change driver of room %s: %w", roomID, err)
	}
	return ev, nil
}

func (s *Store) changeDriver(ctx context.Context, roomID string, expect int64, to, reason string, d envelope.Draft) (envelope.Event, error) {
	if !slices.ContainsFunc(principalKinds, func(p string) bool { return strings.HasPrefix(to, p) && len(to) > len(p) }) {
		return envelope.Event{}, ErrInvalidDriver
	}
	// The payload the change stores, but for its from, known only under the row lock.
	payload := envelope.DriverPayload{To: to, Epoch: expect + 1, Reason: reason}
	d.RoomID, d.Type, d.Payload = roomID, envelope.Driver, envelope.Must(payload)
	if err := d.Validate(); err != nil {
		return envelope.Event{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return envelope.Event{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var from, fallback string
	var epoch int64
	var sealed bool
	err = tx.QueryRow(ctx, `SELECT driver, driver_epoch, fallback_driver, sealed FROM rooms WHERE room_id = $1 FOR UPDATE`,
		roomID).Scan(&from, &epoch, &fallback, &sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return envelope.Event{}, ErrNoRoom
	}
	if err != nil {
		return envelope.Event{}, err
	}
	// Before the epoch check: the retry of a change that won sees the epoch it moved.
	if existing, dup, err := stored(ctx, tx, d); err != nil || dup {
		if err == nil && existing.Type != envelope.Driver {
			return envelope.Event{}, ErrKeyConflict
		}
		return existing, err
	}
	if sealed {
		return envelope.Event{}, ErrSealed
	}
	if epoch != expect {
		return envelope.Event{}, ErrStaleEpoch
	}
	if to == from {
		return envelope.Event{}, ErrInvalidDriver
	}
	if strings.HasPrefix(from, "system:") {
		fallback = from
	}
	if _, err := tx.Exec(ctx, `UPDATE rooms SET driver = $2, driver_epoch = driver_epoch + 1, fallback_driver = $3,
		driver_seen_at = now(), driver_acted_at = now() WHERE room_id = $1`, roomID, to, fallback); err != nil {
		return envelope.Event{}, err
	}
	payload.From = from
	d.Payload = envelope.Must(payload)
	ev, _, err := s.appendTx(ctx, tx, d, fence{})
	if err != nil {
		return envelope.Event{}, err
	}
	return ev, tx.Commit(ctx)
}

// DriverSeen is the holder's heartbeat (every ping) and, with acted, its last action.
// Anyone but the holder is a no-op.
func (s *Store) DriverSeen(ctx context.Context, roomID, principal string, acted bool) error {
	q := `UPDATE rooms SET driver_seen_at = now() WHERE room_id = $1 AND driver = $2`
	if acted {
		q = `UPDATE rooms SET driver_seen_at = now(), driver_acted_at = now() WHERE room_id = $1 AND driver = $2`
	}
	if _, err := s.pool.Exec(ctx, q, roomID, principal); err != nil {
		return fmt.Errorf("store: driver seen in room %s: %w", roomID, err)
	}
	return nil
}

// LapsedDrivers lists human holders disconnected over 2 min or idle over 15 min,
// in open rooms that have a system holder to fall back to.
func (s *Store) LapsedDrivers(ctx context.Context) ([]RoomState, error) {
	rows, err := s.pool.Query(ctx, `SELECT room_id, driver, driver_epoch, fallback_driver FROM rooms
		WHERE NOT sealed AND driver LIKE 'human:%' AND fallback_driver <> ''
		AND (driver_seen_at < now() - interval '2 minutes' OR driver_acted_at < now() - interval '15 minutes')`)
	if err != nil {
		return nil, fmt.Errorf("store: lapsed drivers: %w", err)
	}
	defer rows.Close()
	var out []RoomState
	for rows.Next() {
		var r RoomState
		if err := rows.Scan(&r.ID, &r.Driver, &r.DriverEpoch, &r.FallbackDriver); err != nil {
			return nil, fmt.Errorf("store: lapsed drivers: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LastAck is where a restarted bridge's deliveries resume: the highest ref the
// run's bridge acknowledged as delivered or interrupted, 0 if none. A ref that is
// not a seq is skipped rather than cast, so one bad ack cannot fail every restart.
func (s *Store) LastAck(ctx context.Context, roomID, runID string) (int64, error) {
	var ref int64
	err := s.pool.QueryRow(ctx, `SELECT coalesce(max(CASE WHEN payload->>'ref' ~ '^[0-9]{1,18}$'
		THEN (payload->>'ref')::bigint END), 0) FROM events
		WHERE room_id = $1 AND run_id = $2 AND type = 'state_changed' AND payload->>'kind' IN ('delivered', 'interrupted')`,
		roomID, runID).Scan(&ref)
	return ref, err
}
