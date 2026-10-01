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
	// ErrNotLapsed is a lease expiry of a holder who is not a lapsed human, or to
	// another principal than the room's fallback.
	ErrNotLapsed = errors.New("the holder is no lapsed human")
)

// principalKinds are the prefixes a driver can carry.
var principalKinds = []string{"human:", "system:", "agent:"}

// ChangeDriver moves the token only if the room's epoch is still expect, the
// fence that makes a give, a take and a lease expiry safe across replicas (§2).
// d carries the actor, origin and idempotency key; the store fills the type and
// the DriverPayload. A replayed key returns the stored event and moves nothing.
func (s *Store) ChangeDriver(ctx context.Context, roomID string, expect int64, to, reason string, d envelope.Draft) (envelope.Event, error) {
	ev, err := s.changeDriver(ctx, roomID, expect, to, reason, d, false)
	if err != nil {
		return envelope.Event{}, fmt.Errorf("store: change driver of room %s: %w", roomID, err)
	}
	return ev, nil
}

// ExpireDriver is the lease sweeper's ChangeDriver to the room's fallback, with
// the reason lease_expired. It moves the token only if a human still holds it at
// expect and is still lapsed under the row lock (ErrNotLapsed otherwise): one who
// came back between the sweep's read and this write keeps it.
func (s *Store) ExpireDriver(ctx context.Context, roomID string, expect int64, to string, d envelope.Draft) (envelope.Event, error) {
	ev, err := s.changeDriver(ctx, roomID, expect, to, "lease_expired", d, true)
	if err != nil {
		return envelope.Event{}, fmt.Errorf("store: expire the driver of room %s: %w", roomID, err)
	}
	return ev, nil
}

// lapsedSQL is a human holder's lapse (§2): disconnected over 2 min or idle over 15.
const lapsedSQL = `(driver_seen_at < now() - interval '2 minutes' OR driver_acted_at < now() - interval '15 minutes')`

func (s *Store) changeDriver(ctx context.Context, roomID string, expect int64, to, reason string, d envelope.Draft, expire bool) (envelope.Event, error) {
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
	var sealed, lapsed bool
	err = tx.QueryRow(ctx, `SELECT driver, driver_epoch, fallback_driver, sealed, `+lapsedSQL+` FROM rooms WHERE room_id = $1 FOR UPDATE`,
		roomID).Scan(&from, &epoch, &fallback, &sealed, &lapsed)
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
	if expire && (!lapsed || !strings.HasPrefix(from, "human:") || to != fallback) {
		return envelope.Event{}, ErrNotLapsed
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
		WHERE NOT sealed AND driver LIKE 'human:%' AND fallback_driver <> '' AND `+lapsedSQL)
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

// deliverableTo is the SQL form of bridgeapi.Deliverable for the run named by the
// parameter %[1]s: a steering message addressed to it, or an interrupt of it. Its
// shape must imply the events_deliveries index's predicate, or the index is unused.
const deliverableTo = `((e.type = 'message' AND e.payload->>'delivery' = 'steering' AND e.payload->'to' ? ('agent:' || %[1]s))
	OR (e.type = 'state_changed' AND e.payload->>'kind' = 'interrupt' AND e.payload->>'runId' = %[1]s))`

// lastAckSQL reads the run's acknowledgements through events_acks, and counts a
// ref only if it is a deliverable event of that run: an ack is the bridge's claim
// (design T6), and a forged ref past every delivery skips nothing.
var lastAckSQL = `SELECT coalesce(max(e.seq), 0) FROM events a JOIN events e
	ON e.room_id = a.room_id AND e.seq = CASE WHEN a.payload->>'ref' ~ '^[0-9]{1,18}$' THEN (a.payload->>'ref')::bigint END
	WHERE a.room_id = $1 AND a.run_id = $2 AND a.type = 'state_changed'
	AND a.payload->>'kind' IN ('delivered', 'interrupted', 'undeliverable') AND ` + fmt.Sprintf(deliverableTo, "$2")

// LastAck is where a bridge's deliveries resume: the highest ref the run's bridge
// acknowledged as delivered, interrupted or undeliverable, 0 if none. A ref that
// is not a seq, or not a delivery of this run, is skipped.
func (s *Store) LastAck(ctx context.Context, roomID, runID string) (int64, error) {
	var ref int64
	err := s.pool.QueryRow(ctx, lastAckSQL, roomID, runID).Scan(&ref)
	return ref, err
}

var deliveriesSQL = `SELECT ` + cols + ` FROM events e WHERE e.room_id = $1 AND e.seq > $2 AND e.seq <= $3 AND ` +
	fmt.Sprintf(deliverableTo, "$4") + ` ORDER BY e.seq LIMIT $5`

// Deliveries returns up to limit of the run's deliveries in (after, through], in
// seq order, through events_deliveries: a stream's replay reads only what it may
// send, never the room's whole log (review I3).
func (s *Store) Deliveries(ctx context.Context, roomID, runID string, after, through int64, limit int) ([]envelope.Event, error) {
	rows, err := s.pool.Query(ctx, deliveriesSQL, roomID, after, through, runID, limit)
	if err != nil {
		return nil, fmt.Errorf("store: deliveries of run %s: %w", runID, err)
	}
	defer rows.Close()
	var out []envelope.Event
	for rows.Next() {
		ev, err := scan(rows, roomID)
		if err != nil {
			return nil, fmt.Errorf("store: deliveries of run %s: %w", runID, err)
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}
