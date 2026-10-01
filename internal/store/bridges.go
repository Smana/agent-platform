// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

const (
	// claimAttempts bounds how often ClaimBridge re-decides after the lease moved
	// under it; the caller retries later with ok false.
	claimAttempts = 3
	// liveTimeout bounds one liveness check, on top of the caller's ctx.
	liveTimeout = 5 * time.Second
)

// ClaimBridge takes the room's bridge lease for runID (ruling P17). It lives in the
// room's row, so every broker replica agrees (review I7). Another run keeps it for as
// long as live(holder) is true, however long since its bridge was seen: a quiet or cut-off
// bridge may still have a harness at work, and two runs must never execute in one room
// (ruling SBB, F15). A run that ended frees it at once. live may call the Kubernetes API,
// so it runs outside any transaction, and the takeover that follows is a
// compare-and-swap on the holder it asked about.
func (s *Store) ClaimBridge(ctx context.Context, roomID, runID string, live func(ctx context.Context, runID string) bool) (string, bool, error) {
	var holder string
	for range claimAttempts {
		var took bool
		var err error
		holder, took, err = s.claimFree(ctx, roomID, runID)
		if err != nil {
			return "", false, fmt.Errorf("store: claim the bridge of room %s: %w", roomID, err)
		}
		if took {
			return runID, true, nil
		}
		lctx, cancel := context.WithTimeout(ctx, liveTimeout)
		alive := live(lctx, holder)
		cancel()
		if alive {
			return holder, false, nil
		}
		tag, err := s.pool.Exec(ctx, `UPDATE rooms SET bridge_run = $3, bridge_seen_at = now()
			WHERE room_id = $1 AND bridge_run = $2`, roomID, holder, runID)
		if err != nil {
			return "", false, fmt.Errorf("store: claim the bridge of room %s: %w", roomID, err)
		}
		if tag.RowsAffected() == 1 {
			return runID, true, nil
		}
	}
	return holder, false, nil
}

// claimFree takes the lease when nobody else holds it. Otherwise it returns the
// other holder, with took false and the row lock already released.
func (s *Store) claimFree(ctx context.Context, roomID, runID string) (string, bool, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var holder *string
	err = tx.QueryRow(ctx, `SELECT bridge_run FROM rooms WHERE room_id = $1 FOR UPDATE`, roomID).Scan(&holder)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, ErrNoRoom
	}
	if err != nil {
		return "", false, err
	}
	// SBB: freshness never frees a lease; only live(holder) == false does (ClaimBridge).
	if holder != nil && *holder != runID {
		return *holder, false, nil
	}
	if _, err := tx.Exec(ctx, `UPDATE rooms SET bridge_run = $2, bridge_seen_at = now() WHERE room_id = $1`, roomID, runID); err != nil {
		return "", false, err
	}
	return runID, true, tx.Commit(ctx)
}

// TouchBridge renews the lease of the run that holds it. held is false when another
// run took it: that bridge is displaced, and its appends fail with ErrLeaseLost.
func (s *Store) TouchBridge(ctx context.Context, roomID, runID string) (bool, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE rooms SET bridge_seen_at = now() WHERE room_id = $1 AND bridge_run = $2`, roomID, runID)
	if err != nil {
		return false, fmt.Errorf("store: touch the bridge of room %s: %w", roomID, err)
	}
	return tag.RowsAffected() == 1, nil
}

// IsDataError reports a value PostgreSQL refuses outright (SQLSTATE class 22, such as
// 22P05 for a NUL in jsonb). Retrying cannot help, so the caller stores a stub instead.
func IsDataError(err error) bool {
	var pg *pgconn.PgError
	return errors.As(err, &pg) && len(pg.Code) == 5 && pg.Code[:2] == "22"
}
