// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

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
	Sealed      bool
	ClosedAt    *time.Time
	LastEventAt time.Time
}

// EnsureRoom inserts the room's row once. created is false when it already existed.
// fallback_driver is the system holder a lapsed human driver falls back to (§2);
// a room that starts with a human driver has none until a system principal holds it.
// A retention of zero or less is refused: the Room CRD always defaults it (90d).
func (s *Store) EnsureRoom(ctx context.Context, r NewRoom) (bool, error) {
	if r.Retention <= 0 {
		return false, fmt.Errorf("store: room %s: %w", r.ID, ErrInvalidRetention)
	}
	tag, err := s.pool.Exec(ctx, `INSERT INTO rooms (room_id, driver, fallback_driver, retention)
		VALUES ($1, $2, CASE WHEN $2 LIKE 'system:%' THEN $2 ELSE '' END, make_interval(secs => $3))
		ON CONFLICT (room_id) DO NOTHING`,
		r.ID, r.Driver, r.Retention.Seconds())
	if err != nil {
		return false, fmt.Errorf("store: ensure room %s: %w", r.ID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// Room reads a room's state, or ErrNoRoom.
func (s *Store) Room(ctx context.Context, id string) (RoomState, error) {
	st := RoomState{ID: id}
	err := s.pool.QueryRow(ctx, `SELECT last_seq, driver, driver_epoch, sealed, closed_at, last_event_at
		FROM rooms WHERE room_id = $1`, id).Scan(&st.LastSeq, &st.Driver, &st.DriverEpoch, &st.Sealed, &st.ClosedAt, &st.LastEventAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return st, ErrNoRoom
	}
	return st, err
}

// PendingApprovals counts the room's undecided approvals. Phase 5 migrates the
// approvals table; until then nothing is pending.
func (s *Store) PendingApprovals(ctx context.Context, roomID string) (int, error) {
	var exists bool
	if err := s.pool.QueryRow(ctx, `SELECT to_regclass('public.approvals') IS NOT NULL`).Scan(&exists); err != nil {
		return 0, fmt.Errorf("store: pending approvals of room %s: %w", roomID, err)
	}
	if !exists {
		return 0, nil
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM approvals WHERE room_id = $1 AND state = 'pending'`,
		roomID).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: pending approvals of room %s: %w", roomID, err)
	}
	return n, nil
}
