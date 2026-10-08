// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// MinRetention is the schema's floor on a room's retention (rooms.retention >= 1 day),
// like the Room CRD's <n>d.
const MinRetention = 24 * time.Hour

// NewRoom is what EnsureRoom needs from a Room CR.
type NewRoom struct {
	ID        string
	Driver    string // spec.driver: the initial holder, and the system fallback
	Retention time.Duration
}

// RoomState is a room's row as the broker reads it.
type RoomState struct {
	ID          string
	LastSeq     int64
	Driver      string
	DriverEpoch int64
	// FallbackDriver is the system holder a lapsed human driver falls back to, or "".
	FallbackDriver string
	Sealed         bool
	ClosedAt       *time.Time
	LastEventAt    time.Time
}

// insertRoomSQL creates a room's row ($1 id, $2 driver, $3 retention in seconds);
// every other column takes its default, as broker_create_rooms requires.
const insertRoomSQL = `INSERT INTO rooms (room_id, driver, fallback_driver, retention)
	VALUES ($1, $2, CASE WHEN $2 LIKE 'system:%' THEN $2 ELSE '' END, make_interval(secs => $3))`

// EnsureRoom inserts the room's row once. created is false when it already existed.
// fallback_driver is the system holder a lapsed human driver falls back to (§2);
// a room that starts with a human driver has none until a system principal holds it.
// A retention under MinRetention is refused with ErrInvalidRetention before the
// database, whose check (Ruling AE) would refuse it as a bare 23514.
func (s *Store) EnsureRoom(ctx context.Context, r NewRoom) (bool, error) {
	if r.Retention < MinRetention {
		return false, fmt.Errorf("store: room %s: %w", r.ID, ErrInvalidRetention)
	}
	tag, err := s.pool.Exec(ctx, insertRoomSQL+` ON CONFLICT (room_id) DO NOTHING`, r.ID, r.Driver, r.Retention.Seconds())
	if err != nil {
		return false, fmt.Errorf("store: ensure room %s: %w", r.ID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// Room reads a room's state, or ErrNoRoom.
func (s *Store) Room(ctx context.Context, id string) (RoomState, error) {
	st := RoomState{ID: id}
	err := s.pool.QueryRow(ctx, `SELECT last_seq, driver, driver_epoch, fallback_driver, sealed, closed_at, last_event_at
		FROM rooms WHERE room_id = $1`, id).Scan(&st.LastSeq, &st.Driver, &st.DriverEpoch, &st.FallbackDriver, &st.Sealed,
		&st.ClosedAt, &st.LastEventAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return st, ErrNoRoom
	}
	return st, err
}

// PendingApprovals counts the room's undecided approvals; a sealed room has none
// anyone can decide.
func (s *Store) PendingApprovals(ctx context.Context, roomID string) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM approvals a JOIN rooms r USING (room_id)
		WHERE a.room_id = $1 AND a.state = 'pending' AND NOT r.sealed`, roomID).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: pending approvals of room %s: %w", roomID, err)
	}
	return n, nil
}

// LastTaskState is the payload of the room's highest-seq state_changed{kind:task}, or nil when
// the factory wrote none. The seq decides, so a late or replayed write cannot win.
func (s *Store) LastTaskState(ctx context.Context, roomID string) (json.RawMessage, error) {
	var p json.RawMessage
	err := s.pool.QueryRow(ctx, `SELECT payload FROM events
		WHERE room_id = $1 AND type = 'state_changed' AND payload->>'kind' = 'task'
		ORDER BY seq DESC LIMIT 1`, roomID).Scan(&p)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: last task state of room %s: %w", roomID, err)
	}
	return p, nil
}
