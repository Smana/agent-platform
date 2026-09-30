// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/Smana/agent-platform/internal/envelope"
)

// ErrNotQueued is a state move from a state the queued message is no longer in.
var ErrNotQueued = errors.New("not_queued")

// Queued is one row of a room's FIFO queue; Ref is the queued message's seq.
type Queued struct {
	Ref                 int64
	Author, Text, State string
}

// Enqueue appends a queued message and its FIFO row in one transaction (§2).
// A replayed key returns the stored event and adds no row.
func (s *Store) Enqueue(ctx context.Context, d envelope.Draft, author, text string) (envelope.Event, error) {
	ev, err := s.enqueue(ctx, d, author, text)
	if err != nil {
		return envelope.Event{}, fmt.Errorf("store: enqueue in room %s: %w", d.RoomID, err)
	}
	return ev, nil
}

func (s *Store) enqueue(ctx context.Context, d envelope.Draft, author, text string) (envelope.Event, error) {
	if err := d.Validate(); err != nil {
		return envelope.Event{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return envelope.Event{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	ev, dup, err := s.appendTx(ctx, tx, d, "")
	if err != nil {
		return envelope.Event{}, err
	}
	if dup {
		if ev.Type != envelope.Message {
			return envelope.Event{}, ErrKeyConflict
		}
		return ev, nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO queue (room_id, ref, author, text, state) VALUES ($1, $2, $3, $4, 'queued')`,
		d.RoomID, ev.Seq, author, text); err != nil {
		return envelope.Event{}, err
	}
	return ev, tx.Commit(ctx)
}

// Queue lists the room's messages still queued, oldest first.
func (s *Store) Queue(ctx context.Context, roomID string) ([]Queued, error) {
	rows, err := s.pool.Query(ctx, `SELECT ref, author, text, state FROM queue WHERE room_id = $1 AND state = 'queued' ORDER BY ref`, roomID)
	if err != nil {
		return nil, fmt.Errorf("store: queue of room %s: %w", roomID, err)
	}
	defer rows.Close()
	var out []Queued
	for rows.Next() {
		var q Queued
		if err := rows.Scan(&q.Ref, &q.Author, &q.Text, &q.State); err != nil {
			return nil, fmt.Errorf("store: queue of room %s: %w", roomID, err)
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// SetQueued moves a queued message from one state to another, or ErrNotQueued when
// it is no longer in from. A non-empty runID records the run whose brief took it.
func (s *Store) SetQueued(ctx context.Context, roomID string, ref int64, from, to, runID string) error {
	var run any
	if runID != "" {
		run = runID
	}
	tag, err := s.pool.Exec(ctx, `UPDATE queue SET state = $4, run_id = coalesce($5::text, run_id)
		WHERE room_id = $1 AND ref = $2 AND state = $3`, roomID, ref, from, to, run)
	if err != nil {
		return fmt.Errorf("store: move queued %d of room %s: %w", ref, roomID, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotQueued
	}
	return nil
}
