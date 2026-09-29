package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ClaimBridge takes the room's bridge lease for runID (ruling P17). It lives in the
// room's row, so every broker replica agrees (review I7). Another run keeps it while
// live(holder) is true and it was seen within stale; a run that ended frees it at once.
func (s *Store) ClaimBridge(ctx context.Context, roomID, runID string, stale time.Duration, live func(runID string) bool) (string, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var holder *string
	var fresh bool
	err = tx.QueryRow(ctx, `SELECT bridge_run, coalesce(bridge_seen_at > now() - make_interval(secs => $2), false)
		FROM rooms WHERE room_id = $1 FOR UPDATE`, roomID, stale.Seconds()).Scan(&holder, &fresh)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, ErrNoRoom
	}
	if err != nil {
		return "", false, err
	}
	if holder != nil && *holder != runID && fresh && live(*holder) {
		return *holder, false, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE rooms SET bridge_run = $2, bridge_seen_at = now() WHERE room_id = $1`, roomID, runID); err != nil {
		return "", false, err
	}
	return runID, true, tx.Commit(ctx)
}

// TouchBridge renews the lease of the run that holds it; anyone else's push renews nothing.
func (s *Store) TouchBridge(ctx context.Context, roomID, runID string) error {
	_, err := s.pool.Exec(ctx, `UPDATE rooms SET bridge_seen_at = now() WHERE room_id = $1 AND bridge_run = $2`, roomID, runID)
	return err
}

// IsDataError reports a value PostgreSQL refuses outright (SQLSTATE class 22, such as
// 22P05 for a NUL in jsonb). Retrying cannot help, so the caller stores a stub instead.
func IsDataError(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && len(pg.Code) == 5 && pg.Code[:2] == "22"
}
