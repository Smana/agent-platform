// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"maps"

	"github.com/jackc/pgx/v5"

	"github.com/Smana/agent-platform/internal/envelope"
)

// ErrBadSeq is a fork point outside the source room's log.
var ErrBadSeq = errors.New("bad_seq")

// forkBatch is how many events one round trip copies, two statements each.
const forkBatch = 256

// The copy of one event goes through the schema's own rules, like an append: the
// room takes the next seq, then the event takes it (events_take_next_seq). A
// missing source event leaves a seq without one, which rooms_seq_has_event
// refuses at commit.
const (
	forkBumpSQL = `UPDATE rooms SET last_seq = last_seq + 1, last_event_at = now(),
		bytes = bytes + (SELECT octet_length(payload::text) FROM events WHERE room_id = $1 AND seq = $3)
		WHERE room_id = $2`
	forkCopySQL = `INSERT INTO events (room_id, seq, id, run_id, actor_kind, actor_id, actor_role, type,
		caused_by, origin, origin_client, origin_seq, ts, redactions, payload)
		SELECT $2, seq, id, run_id, actor_kind, actor_id, actor_role, type, caused_by, origin,
		origin_client, origin_seq, ts, redactions, payload FROM events WHERE room_id = $1 AND seq = $3`
)

// Fork creates room r from events 1..upTo of src, copied with their seq, id and
// idempotency key (§5), then appends d as state_changed{forked_from}: fields,
// plus the source room and seq. The copy is the fork's own, so it outlives src's
// purge, and a copied key deduplicates the Room controller's seq-1 Open. It runs
// as the broker's role, under the grants and triggers every append meets: the
// row starts empty, and nothing in src changes, a sealed src included.
func (s *Store) Fork(ctx context.Context, src string, upTo int64, r NewRoom, d envelope.Draft, fields map[string]any) (envelope.Event, error) {
	if r.Retention < MinRetention {
		return envelope.Event{}, fmt.Errorf("store: fork room %s: %w", r.ID, ErrInvalidRetention)
	}
	f := map[string]any{}
	maps.Copy(f, fields)
	f["room"], f["seq"] = src, upTo
	d.RoomID, d.Type, d.Payload = r.ID, envelope.StateChanged, envelope.StatePayload("forked_from", f)
	if err := d.Validate(); err != nil {
		return envelope.Event{}, fmt.Errorf("store: fork room %s: %w", r.ID, err)
	}
	ev, err := s.fork(ctx, src, upTo, r, d)
	if err != nil {
		return envelope.Event{}, fmt.Errorf("store: fork room %s at %d into %s: %w", src, upTo, r.ID, err)
	}
	return ev, nil
}

func (s *Store) fork(ctx context.Context, src string, upTo int64, r NewRoom, d envelope.Draft) (envelope.Event, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return envelope.Event{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// No lock on src: events 1..last_seq never change, and nothing here writes it.
	var last int64
	err = tx.QueryRow(ctx, `SELECT last_seq FROM rooms WHERE room_id = $1`, src).Scan(&last)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return envelope.Event{}, ErrNoRoom
	case err != nil:
		return envelope.Event{}, err
	case upTo < 1 || upTo > last:
		return envelope.Event{}, ErrBadSeq
	}
	if _, err := tx.Exec(ctx, insertRoomSQL, r.ID, r.Driver, r.Retention.Seconds()); err != nil {
		return envelope.Event{}, err
	}
	for from := int64(1); from <= upTo; from += forkBatch {
		b := &pgx.Batch{}
		for seq := from; seq < from+forkBatch && seq <= upTo; seq++ {
			b.Queue(forkBumpSQL, src, r.ID, seq)
			b.Queue(forkCopySQL, src, r.ID, seq)
		}
		if err := tx.SendBatch(ctx, b).Close(); err != nil {
			return envelope.Event{}, err
		}
	}
	ev, _, err := s.appendTx(ctx, tx, d, fence{})
	if err != nil {
		return envelope.Event{}, err
	}
	return ev, tx.Commit(ctx)
}
