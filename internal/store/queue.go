// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/Smana/agent-platform/internal/envelope"
)

var (
	// ErrNotQueued is a state move from a state the queued message is no longer in.
	ErrNotQueued = errors.New("not_queued")
	// ErrNotAQueuedMessage is an Enqueue whose draft is not the queued message it
	// records: not a message, not delivery queued, another text or another author.
	ErrNotAQueuedMessage = errors.New("the draft is not this queued message by its actor")
	// ErrNotAuthor is a removal by neither the message's author nor the driver (§2).
	ErrNotAuthor = errors.New("only the author or the driver removes a queued message")
)

// Queued is one row of a room's FIFO queue; Ref is the queued message's seq.
type Queued struct {
	Ref                 int64
	Author, Text, State string
}

// Enqueue appends a queued message and its FIFO row in one transaction (§2).
// author is always d's actor, and text its payload's: anything else is
// ErrNotAQueuedMessage. A replayed key returns the stored event and adds no row;
// a key stored for anything but a queued message is ErrKeyConflict.
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
	var p envelope.MessagePayload
	if d.Type != envelope.Message || json.Unmarshal(d.Payload, &p) != nil || p.Delivery != envelope.DeliveryQueued ||
		p.Text != text || author != d.Actor.ID {
		return envelope.Event{}, ErrNotAQueuedMessage
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return envelope.Event{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	ev, dup, err := s.appendTx(ctx, tx, d, fence{})
	if err != nil {
		return envelope.Event{}, err
	}
	if dup {
		// Only a queued message has its row: a plain message's key is not an enqueue.
		var queued bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM queue WHERE room_id = $1 AND ref = $2)`,
			d.RoomID, ev.Seq).Scan(&queued); err != nil {
			return envelope.Event{}, err
		}
		if !queued {
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

// SetQueued moves a queued message from one state to another: ErrNotQueued when
// it is no longer in from, ErrSealed once its room is sealed. A non-empty runID
// records the run whose brief took it.
func (s *Store) SetQueued(ctx context.Context, roomID string, ref int64, from, to, runID string) error {
	var run any
	if runID != "" {
		run = runID
	}
	tag, err := s.pool.Exec(ctx, `UPDATE queue SET state = $4, run_id = coalesce($5::text, run_id)
		WHERE room_id = $1 AND ref = $2 AND state = $3 AND room_id IN (SELECT room_id FROM rooms WHERE NOT sealed)`,
		roomID, ref, from, to, run)
	if err != nil {
		return fmt.Errorf("store: move queued %d of room %s: %w", ref, roomID, err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}
	var sealed bool
	if err := s.pool.QueryRow(ctx, `SELECT sealed FROM rooms WHERE room_id = $1`, roomID).Scan(&sealed); err == nil && sealed {
		return ErrSealed
	}
	return ErrNotQueued
}

// lockRoom takes the room's row lock and reads what a queue move decides on.
func lockRoom(ctx context.Context, tx pgx.Tx, roomID string) (sealed bool, driver string, err error) {
	err = tx.QueryRow(ctx, `SELECT sealed, driver FROM rooms WHERE room_id = $1 FOR UPDATE`, roomID).Scan(&sealed, &driver)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, "", ErrNoRoom
	}
	return sealed, driver, err
}

// replay returns the event a retried queue move stored, if any: dup is false for a
// fresh key, and a key stored for anything but this move of ref is ErrKeyConflict.
func replay(ctx context.Context, tx pgx.Tx, d envelope.Draft, ref int64, want envelope.Type) (envelope.Event, bool, error) {
	existing, dup, err := stored(ctx, tx, d)
	if err != nil || !dup {
		return envelope.Event{}, false, err
	}
	if existing.Type != want || !movedRef(existing, ref) {
		return envelope.Event{}, false, ErrKeyConflict
	}
	return existing, true, nil
}

// movedRef reports whether ev is a move of the queued message ref: the steering
// message it caused, or its queued_removed record.
func movedRef(ev envelope.Event, ref int64) bool {
	if ev.CausedBy != nil {
		return *ev.CausedBy == ref
	}
	var p struct {
		Kind string `json:"kind"`
		Ref  int64  `json:"ref"`
	}
	return json.Unmarshal(ev.Payload, &p) == nil && p.Kind == "queued_removed" && p.Ref == ref
}

// PromoteQueued turns a queued message into steering for runID, in one transaction
// fenced on the driver token (§2): the row moves to promoted and the steering
// message, caused by it, is appended together, or neither is. d carries the actor,
// origin and key; the store fills the type, the cause and the payload.
func (s *Store) PromoteQueued(ctx context.Context, roomID string, ref int64, driver string, epoch int64, runID string, d envelope.Draft) (envelope.Event, error) {
	ev, err := s.promote(ctx, roomID, ref, driver, epoch, runID, d)
	if err != nil {
		return envelope.Event{}, fmt.Errorf("store: promote queued %d of room %s: %w", ref, roomID, err)
	}
	return ev, nil
}

func (s *Store) promote(ctx context.Context, roomID string, ref int64, driver string, epoch int64, runID string, d envelope.Draft) (envelope.Event, error) {
	if driver == "" || !envelope.ValidID(runID) {
		return envelope.Event{}, errors.New("a promotion names its driver and a C2 run")
	}
	d.RoomID, d.Type, d.CausedBy = roomID, envelope.Message, &ref
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return envelope.Event{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	sealed, _, err := lockRoom(ctx, tx, roomID)
	if err != nil {
		return envelope.Event{}, err
	}
	if existing, dup, err := replay(ctx, tx, d, ref, envelope.Message); err != nil || dup {
		return existing, err
	}
	if sealed {
		return envelope.Event{}, ErrSealed
	}
	var text string
	err = tx.QueryRow(ctx, `SELECT text FROM queue WHERE room_id = $1 AND ref = $2 AND state = 'queued' FOR UPDATE`,
		roomID, ref).Scan(&text)
	if errors.Is(err, pgx.ErrNoRows) {
		return envelope.Event{}, ErrNotQueued
	}
	if err != nil {
		return envelope.Event{}, err
	}
	d.Payload = envelope.Must(envelope.MessagePayload{Kind: envelope.KindChat, Text: text,
		To: []string{"agent:" + runID}, Delivery: envelope.DeliverySteering})
	if err := d.Validate(); err != nil {
		return envelope.Event{}, err
	}
	if _, err := tx.Exec(ctx, `UPDATE queue SET state = 'promoted', run_id = $3 WHERE room_id = $1 AND ref = $2`,
		roomID, ref, runID); err != nil {
		return envelope.Event{}, err
	}
	ev, _, err := s.appendTx(ctx, tx, d, fence{driver: driver, epoch: epoch})
	if err != nil {
		return envelope.Event{}, err
	}
	return ev, tx.Commit(ctx)
}

// RemoveQueued withdraws a queued message for d's actor, its author or the driver
// (ErrNotAuthor otherwise), and records state_changed{queued_removed} in the same
// transaction. The store fills d's type and payload.
func (s *Store) RemoveQueued(ctx context.Context, roomID string, ref int64, d envelope.Draft) (envelope.Event, error) {
	ev, err := s.remove(ctx, roomID, ref, d)
	if err != nil {
		return envelope.Event{}, fmt.Errorf("store: remove queued %d of room %s: %w", ref, roomID, err)
	}
	return ev, nil
}

func (s *Store) remove(ctx context.Context, roomID string, ref int64, d envelope.Draft) (envelope.Event, error) {
	d.RoomID, d.Type = roomID, envelope.StateChanged
	d.Payload = envelope.StatePayload("queued_removed", map[string]any{"ref": ref})
	if err := d.Validate(); err != nil {
		return envelope.Event{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return envelope.Event{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	sealed, driver, err := lockRoom(ctx, tx, roomID)
	if err != nil {
		return envelope.Event{}, err
	}
	if existing, dup, err := replay(ctx, tx, d, ref, envelope.StateChanged); err != nil || dup {
		return existing, err
	}
	if sealed {
		return envelope.Event{}, ErrSealed
	}
	var author string
	err = tx.QueryRow(ctx, `SELECT author FROM queue WHERE room_id = $1 AND ref = $2 AND state = 'queued' FOR UPDATE`,
		roomID, ref).Scan(&author)
	if errors.Is(err, pgx.ErrNoRows) {
		return envelope.Event{}, ErrNotQueued
	}
	if err != nil {
		return envelope.Event{}, err
	}
	if author != d.Actor.ID && driver != d.Actor.ID {
		return envelope.Event{}, ErrNotAuthor
	}
	if _, err := tx.Exec(ctx, `UPDATE queue SET state = 'removed' WHERE room_id = $1 AND ref = $2`, roomID, ref); err != nil {
		return envelope.Event{}, err
	}
	ev, _, err := s.appendTx(ctx, tx, d, fence{})
	if err != nil {
		return envelope.Event{}, err
	}
	return ev, tx.Commit(ctx)
}
