package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

type NewRoom struct {
	ID        string
	Driver    string // spec.driver: the initial holder, and the system fallback
	Retention time.Duration
}

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
func (s *Store) EnsureRoom(ctx context.Context, r NewRoom) (bool, error) {
	tag, err := s.pool.Exec(ctx, `INSERT INTO rooms (room_id, driver, fallback_driver, retention)
		VALUES ($1, $2, CASE WHEN $2 LIKE 'system:%' THEN $2 ELSE '' END, make_interval(secs => $3))
		ON CONFLICT (room_id) DO NOTHING`,
		r.ID, r.Driver, r.Retention.Seconds())
	return tag.RowsAffected() == 1, err
}

func (s *Store) Room(ctx context.Context, id string) (RoomState, error) {
	st := RoomState{ID: id}
	err := s.pool.QueryRow(ctx, `SELECT last_seq, driver, driver_epoch, sealed, closed_at, last_event_at
		FROM rooms WHERE room_id = $1`, id).Scan(&st.LastSeq, &st.Driver, &st.DriverEpoch, &st.Sealed, &st.ClosedAt, &st.LastEventAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return st, ErrNoRoom
	}
	return st, err
}
