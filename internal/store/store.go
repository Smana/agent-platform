// Package store is the log of record (SP2 §4): one gapless, append-only
// sequence per room, in PostgreSQL.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/oklog/ulid/v2"

	"github.com/Smana/agent-platform/internal/envelope"
)

var (
	ErrNoRoom = errors.New("no such room")
	ErrSealed = errors.New("room is sealed")
)

type Store struct {
	pool      *pgxpool.Pool
	MaxEvents int64
	MaxBytes  int64
	Now       func() time.Time
}

func Open(ctx context.Context, url string) (*Store, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	return &Store{pool: pool, MaxEvents: 100_000, MaxBytes: 256 << 20, Now: time.Now}, nil
}

func (s *Store) Close()                         { s.pool.Close() }
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// SchemaReady backs /startupz: the Atlas migration has run.
func (s *Store) SchemaReady(ctx context.Context) (bool, error) {
	var ok bool
	err := s.pool.QueryRow(ctx, `SELECT to_regclass('public.events') IS NOT NULL`).Scan(&ok)
	return ok, err
}

const cols = `seq, id, coalesce(run_id, ''), actor_kind, actor_id, coalesce(actor_role, ''), type,
	caused_by, origin, ts, redactions, payload`

func scan(row pgx.Row, roomID string) (envelope.Event, error) {
	var e envelope.Event
	var kind, typ, origin string
	var payload []byte
	err := row.Scan(&e.Seq, &e.ID, &e.RunID, &kind, &e.Actor.ID, &e.Actor.Role, &typ, &e.CausedBy,
		&origin, &e.TS, &e.Redactions, &payload)
	e.V, e.RoomID = envelope.Version, roomID
	e.Actor.Kind, e.Type, e.Origin, e.Payload = envelope.ActorKind(kind), envelope.Type(typ), envelope.Origin(origin), payload
	if e.Redactions == nil {
		e.Redactions = []string{}
	}
	return e, err
}

// Append is one transaction: take the room's row lock by incrementing last_seq,
// return the existing event for a replayed idempotency key (the rollback undoes the
// increment), insert, and seal the room with a final limit event when it is full.
func (s *Store) Append(ctx context.Context, d envelope.Draft) (envelope.Event, bool, error) {
	if err := d.Validate(); err != nil {
		return envelope.Event{}, false, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return envelope.Event{}, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	ev, dup, err := s.appendTx(ctx, tx, d)
	if err != nil || dup {
		return ev, dup, err
	}
	return ev, false, tx.Commit(ctx)
}

func (s *Store) appendTx(ctx context.Context, tx pgx.Tx, d envelope.Draft) (envelope.Event, bool, error) {
	if len(d.Payload) > envelope.MaxPayload {
		d.Payload = envelope.Oversize(d.Type, len(d.Payload))
	}
	var seq, size int64
	var sealed bool
	err := tx.QueryRow(ctx, `UPDATE rooms SET last_seq = last_seq + 1, bytes = bytes + $2, last_event_at = now()
		WHERE room_id = $1 RETURNING last_seq, bytes, sealed`, d.RoomID, len(d.Payload)).Scan(&seq, &size, &sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return envelope.Event{}, false, ErrNoRoom
	}
	if err != nil {
		return envelope.Event{}, false, err
	}
	if sealed {
		return envelope.Event{}, false, ErrSealed
	}
	// The row lock above serialises writers of this room, so this read cannot race.
	existing, err := scan(tx.QueryRow(ctx, `SELECT `+cols+` FROM events
		WHERE room_id = $1 AND origin_client = $2 AND origin_seq = $3`, d.RoomID, d.OriginClient, d.OriginSeq), d.RoomID)
	if err == nil {
		return existing, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return envelope.Event{}, false, err
	}
	ev := envelope.Event{V: envelope.Version, ID: ulid.Make().String(), Seq: seq, RoomID: d.RoomID,
		RunID: d.RunID, Actor: d.Actor, Type: d.Type, CausedBy: d.CausedBy, Origin: d.Origin,
		TS: s.Now().UTC().Truncate(time.Microsecond), Redactions: d.Redactions, Payload: d.Payload}
	if ev.Redactions == nil {
		ev.Redactions = []string{}
	}
	if err := insert(ctx, tx, ev, d.OriginClient, d.OriginSeq); err != nil {
		return envelope.Event{}, false, err
	}
	if seq+1 >= s.MaxEvents || size >= s.MaxBytes {
		if err := s.sealTx(ctx, tx, d.RoomID, "limit", map[string]any{"events": seq + 1, "bytes": size}); err != nil {
			return envelope.Event{}, false, err
		}
	}
	return ev, false, nil
}

func insert(ctx context.Context, tx pgx.Tx, ev envelope.Event, client string, n int64) error {
	var runID, role any
	if ev.RunID != "" {
		runID = ev.RunID
	}
	if ev.Actor.Role != "" {
		role = ev.Actor.Role
	}
	_, err := tx.Exec(ctx, `INSERT INTO events (room_id, seq, id, run_id, actor_kind, actor_id, actor_role,
		type, caused_by, origin, origin_client, origin_seq, ts, redactions, payload)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)`,
		ev.RoomID, ev.Seq, ev.ID, runID, string(ev.Actor.Kind), ev.Actor.ID, role, string(ev.Type),
		ev.CausedBy, string(ev.Origin), client, n, ev.TS, ev.Redactions, []byte(ev.Payload))
	return err
}

// sealTx appends the room's last event and seals it, inside the caller's transaction.
func (s *Store) sealTx(ctx context.Context, tx pgx.Tx, roomID, kind string, fields map[string]any) error {
	var seq int64
	if err := tx.QueryRow(ctx, `UPDATE rooms SET last_seq = last_seq + 1, sealed = true,
		closed_at = coalesce(closed_at, now()) WHERE room_id = $1 RETURNING last_seq`, roomID).Scan(&seq); err != nil {
		return err
	}
	ev := envelope.Event{V: envelope.Version, ID: ulid.Make().String(), Seq: seq, RoomID: roomID,
		Actor: envelope.Actor{Kind: envelope.ActorSystem, ID: "system:room-broker"}, Type: envelope.StateChanged,
		Origin: envelope.OriginBroker, TS: s.Now().UTC().Truncate(time.Microsecond), Redactions: []string{},
		Payload: envelope.StatePayload(kind, fields)}
	return insert(ctx, tx, ev, "broker:seal", 1)
}

// CloseRoom seals the log with a final state_changed{room_phase: Closed}. Its
// retention clock starts now (OD-17). Closing twice is a no-op.
func (s *Store) CloseRoom(ctx context.Context, roomID, reason string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var sealed bool
	if err := tx.QueryRow(ctx, `SELECT sealed FROM rooms WHERE room_id = $1 FOR UPDATE`, roomID).Scan(&sealed); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNoRoom
		}
		return err
	}
	if sealed {
		return nil
	}
	if err := s.sealTx(ctx, tx, roomID, "room_phase", map[string]any{"phase": "Closed", "reason": reason}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Store) Range(ctx context.Context, roomID string, afterSeq int64, limit int) ([]envelope.Event, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+cols+` FROM events WHERE room_id = $1 AND seq > $2
		ORDER BY seq LIMIT $3`, roomID, afterSeq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []envelope.Event
	for rows.Next() {
		ev, err := scan(rows, roomID)
		if err != nil {
			return nil, err
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

func (s *Store) Cursor(ctx context.Context, roomID, originClient string) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `SELECT coalesce(max(origin_seq), 0) FROM events
		WHERE room_id = $1 AND origin_client = $2`, roomID, originClient).Scan(&n)
	return n, err
}

// LastHarnessStatus is the run's last execution_status the bridge mirrored (ruling P15).
func (s *Store) LastHarnessStatus(ctx context.Context, roomID, runID string) (string, error) {
	var status string
	err := s.pool.QueryRow(ctx, `SELECT payload->>'status' FROM events WHERE room_id = $1 AND run_id = $2
		AND type = 'state_changed' AND payload->>'kind' = 'harness_status' ORDER BY seq DESC LIMIT 1`,
		roomID, runID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return status, err
}
