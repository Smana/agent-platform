// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// Subscriber registers a webhook subscriber and returns where it stands (R17, R18), in one
// transaction. On the name's first insert only, every existing room gets a cursor at its
// last_seq, so no history is replayed; a re-added name keeps its row and its cursors. A new
// generation re-enables the subscriber.
func (s *Store) Subscriber(ctx context.Context, sub string, generation int) (failures int, disabled bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `WITH added AS (
		INSERT INTO webhook_subscribers (subscriber, generation) VALUES ($1, $2)
		ON CONFLICT (subscriber) DO NOTHING RETURNING subscriber
	)
	INSERT INTO webhook_cursors (subscriber, room_id, seq)
	SELECT added.subscriber, rooms.room_id, rooms.last_seq FROM added, rooms`, sub, generation); err != nil {
		return 0, false, err
	}
	if _, err := tx.Exec(ctx, `UPDATE webhook_subscribers SET generation = $2, failures = 0, disabled_at = NULL
		WHERE subscriber = $1 AND generation <> $2`, sub, generation); err != nil {
		return 0, false, err
	}
	if err := tx.QueryRow(ctx, `SELECT failures, disabled_at IS NOT NULL FROM webhook_subscribers WHERE subscriber = $1`,
		sub).Scan(&failures, &disabled); err != nil {
		return 0, false, err
	}
	return failures, disabled, tx.Commit(ctx)
}

// WebhookCursor is the last seq of room the subscriber is done with: 0 when there is none, so a room
// created after the subscriber starts at its first event.
func (s *Store) WebhookCursor(ctx context.Context, sub, room string) (int64, error) {
	var seq int64
	err := s.pool.QueryRow(ctx, `SELECT seq FROM webhook_cursors WHERE subscriber = $1 AND room_id = $2`, sub, room).Scan(&seq)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return seq, err
}

// SetWebhookCursor moves the subscriber's cursor in room to seq; it never moves back.
func (s *Store) SetWebhookCursor(ctx context.Context, sub, room string, seq int64) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO webhook_cursors (subscriber, room_id, seq) VALUES ($1, $2, $3)
		ON CONFLICT (subscriber, room_id) DO UPDATE SET seq = GREATEST(webhook_cursors.seq, EXCLUDED.seq)`, sub, room, seq)
	return err
}

// Lagging lists up to limit rooms whose last_seq is past the subscriber's cursor, newest
// activity first. The emitter asks on each reconnect of Listen, since notifications sent while
// the listener was down are gone.
func (s *Store) Lagging(ctx context.Context, sub string, limit int) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT rooms.room_id FROM rooms
		LEFT JOIN webhook_cursors c ON c.subscriber = $1 AND c.room_id = rooms.room_id
		WHERE rooms.last_seq > COALESCE(c.seq, 0) ORDER BY rooms.last_event_at DESC LIMIT $2`, sub, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// Failed counts one given-up delivery and returns the streak.
func (s *Store) Failed(ctx context.Context, sub string) (failures int, err error) {
	err = s.pool.QueryRow(ctx, `UPDATE webhook_subscribers SET failures = failures + 1
		WHERE subscriber = $1 RETURNING failures`, sub).Scan(&failures)
	return failures, err
}

// Succeeded ends the failure streak.
func (s *Store) Succeeded(ctx context.Context, sub string) error {
	_, err := s.pool.Exec(ctx, `UPDATE webhook_subscribers SET failures = 0 WHERE subscriber = $1 AND failures <> 0`, sub)
	return err
}

// Disable stops deliveries to the subscriber until its generation changes.
func (s *Store) Disable(ctx context.Context, sub string) error {
	_, err := s.pool.Exec(ctx, `UPDATE webhook_subscribers SET disabled_at = now() WHERE subscriber = $1`, sub)
	return err
}
