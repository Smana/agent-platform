// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// expired is a room sealed longer ago than its retention (OD-17). The retention
// role's row-level security exposes only those for DELETE: this clause is the
// belt, RLS the braces.
const expired = `sealed AND closed_at IS NOT NULL AND closed_at < now() - retention`

// PurgeExpired deletes every expired room's log, then its row, one room per
// transaction, so each stays bounded by the room limits (100 000 events) under
// the session's statement_timeout. It runs as rooms_retention; the broker's role
// holds no DELETE and fails. Every table referencing rooms (queue; approvals in
// phase 5) is deleted before the room's row.
func (s *Store) PurgeExpired(ctx context.Context) (rooms, events int64, err error) {
	ids, err := s.expiredRooms(ctx)
	if err != nil {
		return 0, 0, fmt.Errorf("store: list expired rooms: %w", err)
	}
	for _, id := range ids {
		n, err := s.purge(ctx, id)
		if err != nil {
			return rooms, events, fmt.Errorf("store: purge room %s: %w", id, err)
		}
		rooms, events = rooms+1, events+n
	}
	return rooms, events, nil
}

func (s *Store) expiredRooms(ctx context.Context) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT room_id FROM rooms WHERE `+expired+` ORDER BY closed_at`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

func (s *Store) purge(ctx context.Context, roomID string) (int64, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// queue references events, so it goes first.
	if _, err := tx.Exec(ctx, `DELETE FROM queue WHERE room_id = $1
		AND room_id IN (SELECT room_id FROM rooms WHERE `+expired+`)`, roomID); err != nil {
		return 0, err
	}
	tag, err := tx.Exec(ctx, `DELETE FROM events WHERE room_id = $1
		AND room_id IN (SELECT room_id FROM rooms WHERE `+expired+`)`, roomID)
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM rooms WHERE room_id = $1 AND `+expired, roomID); err != nil {
		return 0, err
	}
	return tag.RowsAffected(), tx.Commit(ctx)
}
