// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

const (
	sealedRoom = "sealedaa"
	// A legitimate append done by hand: the next seq, then its event.
	handAppend = `UPDATE rooms SET last_seq = last_seq + 1 WHERE room_id = '3kq7x2ma';
		INSERT INTO events (room_id, seq, id, actor_kind, actor_id, type, origin, origin_client, origin_seq, ts, payload)
		SELECT room_id, last_seq, 'hand', 'system', 'system:test', 'message', 'broker', 'test:hand', 1, now(), '{}'
		FROM rooms WHERE room_id = '3kq7x2ma'`
	// A driver move by hand, from epoch 0; its driver event must follow.
	moveDriver = `UPDATE rooms SET driver = 'human:mallory', driver_epoch = driver_epoch + 1 WHERE room_id = '3kq7x2ma';
		`
)

// driverEvent is handAppend for a driver event with this payload, under key test:hand/n.
func driverEvent(payload string, n int) string {
	return strings.NewReplacer("'message'", "'driver'", "'{}'", "'"+payload+"'",
		"'test:hand', 1", fmt.Sprintf("'test:hand', %d", n)).Replace(handAppend)
}

func eventInto(roomID, seq string) string {
	return `INSERT INTO events (room_id, seq, id, actor_kind, actor_id, type, origin, origin_client, origin_seq, ts, payload)
		SELECT room_id, ` + seq + `, 'forged', 'system', 'system:test', 'message', 'broker', 'test:forged', 1, now(), '{}'
		FROM rooms WHERE room_id = '` + roomID + `'`
}

// SC-10, T12, Ruling Y: the broker role can append and move a room forward, and
// nothing else. The database refuses every rewrite, even one the store never issues.
func TestBrokerRoleIsAppendOnly(t *testing.T) {
	s, _, _, _ := open(t)
	ctx := context.Background()
	for i := int64(1); i <= 2; i++ {
		if _, _, err := s.Append(ctx, draft("agent:x", i)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.EnsureRoom(ctx, NewRoom{ID: sealedRoom, Driver: "system:factory", Retention: 24 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{room, sealedRoom} { // one queued message in each
		q := queuedDraft(1, "queued")
		q.RoomID = id
		if _, err := s.Enqueue(ctx, q, "human:alice", "queued"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CloseRoom(ctx, sealedRoom, "sealed for the test"); err != nil {
		t.Fatal(err)
	}
	const (
		queuedRef = `(SELECT max(ref) FROM queue WHERE room_id = '3kq7x2ma')`
		queueRow  = `INSERT INTO queue (room_id, ref, author, text, state) VALUES `
	)

	for _, tc := range []struct {
		name, sql, code, msg string
	}{
		{"update an event", `UPDATE events SET payload = '{}'`, "42501", ""},
		{"delete an event", `DELETE FROM events`, "42501", ""},
		{"truncate the log", `TRUNCATE events`, "42501", ""},
		{"delete a room", `DELETE FROM rooms`, "42501", ""},
		{"shorten retention", `UPDATE rooms SET retention = '0' WHERE room_id = '3kq7x2ma'`, "42501", ""},
		{"backdate and shorten together", `UPDATE rooms SET closed_at = now() - interval '1000 days', retention = '0' WHERE room_id = '3kq7x2ma'`, "42501", ""},
		{"create a sealed room", `INSERT INTO rooms (room_id, driver, fallback_driver, sealed) VALUES ('zzzzzzzz', 'x', '', true)`, "42501", ""},
		{"create a room with no retention", `INSERT INTO rooms (room_id, driver, fallback_driver, retention) VALUES ('zzzzzzzz', 'x', '', '0')`, "23514", "retention"},
		{"backdate a close", `UPDATE rooms SET closed_at = now() - interval '1000 days' WHERE room_id = '3kq7x2ma'`, "23514", "closed_at"},
		{"close without sealing", `UPDATE rooms SET closed_at = now() WHERE room_id = '3kq7x2ma'`, "23514", "closed_at"},
		{"move a close date", `UPDATE rooms SET closed_at = now() - interval '1000 days' WHERE room_id = 'sealedaa'`, "23514", "closed_at"},
		{"reopen a sealed room", `UPDATE rooms SET sealed = false WHERE room_id = 'sealedaa'`, "23514", "sealed"},
		{"jump last_seq", `UPDATE rooms SET last_seq = last_seq + 100 WHERE room_id = '3kq7x2ma'`, "23514", "last_seq"},
		{"rewind last_seq", `UPDATE rooms SET last_seq = last_seq - 1 WHERE room_id = '3kq7x2ma'`, "23514", "last_seq"},
		{"skip a seq", `UPDATE rooms SET last_seq = last_seq + 1 WHERE room_id = '3kq7x2ma'`, "23514", "no event"},
		{"skip a seq behind a temporary table", `CREATE TEMP TABLE events (room_id text, seq bigint) ON COMMIT DROP;
			INSERT INTO events SELECT room_id, last_seq + 1 FROM rooms WHERE room_id = '3kq7x2ma';
			UPDATE rooms SET last_seq = last_seq + 1 WHERE room_id = '3kq7x2ma'`, "23514", "no event"},
		{"shrink bytes", `UPDATE rooms SET bytes = 0 WHERE room_id = '3kq7x2ma'`, "23514", "bytes"},
		{"advance a sealed room", `UPDATE rooms SET last_seq = last_seq + 1 WHERE room_id = 'sealedaa'`, "23514", "sealed"},
		{"insert out of sequence", eventInto(room, "99"), "23514", "next"},
		{"insert into a sealed room", eventInto(sealedRoom, "last_seq + 1"), "23514", "sealed"},
		{"move the driver without its epoch", `UPDATE rooms SET driver = 'human:mallory' WHERE room_id = '3kq7x2ma'`, "23514", "epoch"},
		{"move the fallback without its epoch", `UPDATE rooms SET fallback_driver = 'human:mallory' WHERE room_id = '3kq7x2ma'`, "23514", "epoch"},
		{"jump the driver epoch", `UPDATE rooms SET driver_epoch = driver_epoch + 2 WHERE room_id = '3kq7x2ma'`, "23514", "driver_epoch"},
		{"rewind the driver epoch", `UPDATE rooms SET driver_epoch = driver_epoch - 1 WHERE room_id = '3kq7x2ma'`, "23514", "driver_epoch"},
		{"move the driver off the record", `UPDATE rooms SET driver = 'human:mallory', driver_epoch = driver_epoch + 1 WHERE room_id = '3kq7x2ma'`, "23514", "no driver event"},
		{"move the driver behind another event", moveDriver + handAppend, "23514", "no driver event"},
		{"move the driver behind another epoch's event", moveDriver + driverEvent(`{"epoch": 99, "from": "system:factory", "to": "human:mallory"}`, 2), "23514", "no driver event"},
		{"move the driver behind an event naming another holder", moveDriver + driverEvent(`{"epoch": 1, "from": "system:factory", "to": "human:alice"}`, 2), "23514", "no driver event"},
		{"move the driver behind an event naming another previous holder", moveDriver + driverEvent(`{"epoch": 1, "from": "human:x", "to": "human:mallory"}`, 2), "23514", "no driver event"},
		// Review I1: the event must follow the move, or one planted earlier would cover it.
		{"move the driver behind an earlier driver event", driverEvent(`{"epoch": 1, "from": "system:factory", "to": "human:mallory"}`, 3) + ";\n" + moveDriver, "23514", "no driver event"},
		{"move the fallback to a human", `UPDATE rooms SET driver = 'human:mallory', fallback_driver = 'human:mallory', driver_epoch = driver_epoch + 1
			WHERE room_id = '3kq7x2ma'`, "23514", "previous system holder"},
		// Committed: the room is at epoch 1 from here on.
		{"move the driver with its event by hand", moveDriver + driverEvent(`{"epoch": 1, "from": "system:factory", "to": "human:mallory"}`, 4), "", ""},
		{"keep the system fallback on a human-to-human move", `UPDATE rooms SET driver = 'human:alice', fallback_driver = 'human:mallory', driver_epoch = driver_epoch + 1
			WHERE room_id = '3kq7x2ma'`, "23514", "previous system holder"},
		{"queue a message with no event", queueRow + `('3kq7x2ma', 9999, 'human:bob', 'planted', 'queued')`, "23514", "not its queued message"},
		{"queue a plain message", queueRow + `('3kq7x2ma', 1, 'agent:7f3cq2xz', 'm1', 'queued')`, "23514", "not its queued message"},
		{"queue someone's message as another's", queueRow + `('3kq7x2ma', ` + queuedRef + `, 'human:mallory', 'queued', 'queued')`, "23514", "not its queued message"},
		{"queue a message with other text", queueRow + `('3kq7x2ma', ` + queuedRef + `, 'human:alice', 'planted', 'queued')`, "23514", "not its queued message"},
		{"queue into a sealed room", queueRow + `('sealedaa', 1, 'human:alice', 'queued', 'queued')`, "23514", "queue stays"},
		{"move a sealed room's queue", `UPDATE queue SET state = 'removed' WHERE room_id = 'sealedaa'`, "23514", "queue stays"},
		{"consume by a run that is no run id", `UPDATE queue SET state = 'consumed', run_id = 'NOT A RUN' WHERE room_id = '3kq7x2ma'`, "23514", "run_id"},
		{"consume without naming the run", `UPDATE queue SET state = 'consumed' WHERE room_id = '3kq7x2ma'`, "23514", "names a run"},
		{"remove naming a run", `UPDATE queue SET state = 'removed', run_id = '7f3cq2xz' WHERE room_id = '3kq7x2ma'`, "23514", "names a run"},
		{"re-queue a queued message", `UPDATE queue SET state = 'queued' WHERE room_id = '3kq7x2ma'`, "23514", "moves once"},
		{"name a run without moving", `UPDATE queue SET run_id = '7f3cq2xz' WHERE room_id = '3kq7x2ma'`, "23514", "moves once"},
		{"move a queued message", `UPDATE queue SET state = 'removed' WHERE room_id = '3kq7x2ma'`, "", ""},
		{"move a removed message again", `UPDATE queue SET state = 'consumed', run_id = '7f3cq2xz' WHERE room_id = '3kq7x2ma'`, "23514", "moves once"},
		{"move a sealed room's driver", `UPDATE rooms SET driver = 'human:mallory', driver_epoch = driver_epoch + 1 WHERE room_id = 'sealedaa'`, "23514", "driver stays"},
		{"rewrite a queued message", `UPDATE queue SET text = 'forged'`, "42501", ""},
		{"re-attribute a queued message", `UPDATE queue SET author = 'human:mallory'`, "42501", ""},
		{"delete a queued message", `DELETE FROM queue`, "42501", ""},
		{"a legitimate append by hand", handAppend, "", ""},
		{"a driver heartbeat", `UPDATE rooms SET driver_seen_at = now(), driver_acted_at = now() WHERE room_id = '3kq7x2ma'`, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := inTx(ctx, s, tc.sql)
			if tc.code == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			var pg *pgconn.PgError
			if !errors.As(err, &pg) || pg.Code != tc.code || !strings.Contains(pg.Message, tc.msg) {
				t.Fatalf("want SQLSTATE %s mentioning %q, got %v", tc.code, tc.msg, err)
			}
		})
	}
	if !gapless(t, s, room) {
		t.Fatal("the log has a gap")
	}
}

// inTx runs sql as the broker in one transaction, so deferred checks run at commit.
func inTx(ctx context.Context, s *Store, sql string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, sql); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// Ruling AX: the retention role reads only what purging needs, the room ids and
// the four columns of the expiry predicate, and only for expired rooms. It never
// reads a transcript, not even one it is about to delete.
func TestRetentionRoleCannotReadTranscripts(t *testing.T) {
	s, _, retention, super := open(t)
	ctx := context.Background()
	for _, id := range []string{"openaaaa", "expiredx"} {
		if _, err := s.EnsureRoom(ctx, NewRoom{ID: id, Driver: "system:factory", Retention: 24 * time.Hour}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.Append(ctx, draftIn(id, "agent:x", 1)); err != nil {
			t.Fatal(err)
		}
		q := queuedDraft(1, "queued text")
		q.RoomID = id
		if _, err := s.Enqueue(ctx, q, "human:alice", "queued text"); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CloseRoom(ctx, "expiredx", "done"); err != nil {
		t.Fatal(err)
	}
	forge(t, super, `UPDATE rooms SET closed_at = now() - interval '2 days' WHERE room_id = 'expiredx'`)
	var held int // the expired room's events: its append and its close
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE room_id = 'expiredx'`).Scan(&held); err != nil {
		t.Fatal(err)
	}
	r, err := Open(ctx, retention)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	for _, tc := range []struct{ name, sql string }{
		{"an event's payload", `SELECT payload FROM events`},
		{"an event's actor", `SELECT actor_id FROM events`},
		{"a whole event", `SELECT * FROM events`},
		{"a room's driver", `SELECT driver FROM rooms`},
		{"a queued message's text", `SELECT text FROM queue`},
		{"a queued message's author", `SELECT author FROM queue`},
	} {
		t.Run("refused: "+tc.name, func(t *testing.T) {
			_, err := r.pool.Exec(ctx, tc.sql)
			if pg, ok := errors.AsType[*pgconn.PgError](err); !ok || pg.Code != "42501" {
				t.Fatalf("want SQLSTATE 42501, got %v", err)
			}
		})
	}
	for _, tc := range []struct {
		name, sql string
		want      int
	}{
		{"a live room's events are invisible", `SELECT count(*) FROM events WHERE room_id = 'openaaaa'`, 0},
		{"a live room's row is invisible", `SELECT count(*) FROM rooms WHERE room_id = 'openaaaa'`, 0},
		{"an expired room's row is visible", `SELECT count(*) FROM rooms WHERE room_id = 'expiredx'`, 1},
		{"an expired room's events are countable", `SELECT count(*) FROM events WHERE room_id = 'expiredx'`, held},
		{"a live room's queue is invisible", `SELECT count(*) FROM queue WHERE room_id = 'openaaaa'`, 0},
		{"an expired room's queue is countable", `SELECT count(*) FROM queue WHERE room_id = 'expiredx'`, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var n int
			if err := r.pool.QueryRow(ctx, tc.sql).Scan(&n); err != nil || n != tc.want {
				t.Fatalf("count = %d, %v; want %d", n, err, tc.want)
			}
		})
	}
	// The queue references its events: they cannot go first (review 4.1 M3).
	_, err = r.pool.Exec(ctx, `DELETE FROM events WHERE room_id = 'expiredx'`)
	if pg, ok := errors.AsType[*pgconn.PgError](err); !ok || pg.Code != "23503" {
		t.Fatalf("deleting queued events before their queue rows: want SQLSTATE 23503, got %v", err)
	}
	if rooms, events, err := r.PurgeExpired(ctx); err != nil || rooms != 1 || events != int64(held) {
		t.Fatalf("purge under the narrowed role: %d rooms, %d events, %v", rooms, events, err)
	}
}

// OD-17: the retention role deletes a room's log only once it is sealed and its
// close date is older than its retention.
func TestRetentionDeletesOnlyExpiredSealedRooms(t *testing.T) {
	s, _, retention, super := open(t)
	ctx := context.Background()
	rooms := map[string]bool{ // room -> purged
		"openaaaa": false, // open, with events
		"unsealed": false, // a close date past retention, forged, but never sealed
		"freshaaa": false, // sealed within retention
		"expiredx": true,  // sealed, past retention
	}
	for id := range rooms {
		if _, err := s.EnsureRoom(ctx, NewRoom{ID: id, Driver: "system:factory", Retention: 90 * 24 * time.Hour}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.Append(ctx, draftIn(id, "agent:x", 1)); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"freshaaa", "expiredx"} {
		if err := s.CloseRoom(ctx, id, "done"); err != nil {
			t.Fatal(err)
		}
	}
	forge(t, super, `UPDATE rooms SET closed_at = now() - interval '91 days' WHERE room_id IN ('unsealed', 'expiredx')`)

	r, err := Open(ctx, retention)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.pool.Exec(ctx, `DELETE FROM events`); err != nil {
		t.Fatalf("retention delete of events: %v", err)
	}
	if _, err := r.pool.Exec(ctx, `DELETE FROM rooms`); err != nil {
		t.Fatalf("retention delete of rooms: %v", err)
	}
	for id, purged := range rooms {
		t.Run(id, func(t *testing.T) {
			var events, roomRows int
			if err := s.pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM events WHERE room_id = $1),
				(SELECT count(*) FROM rooms WHERE room_id = $1)`, id).Scan(&events, &roomRows); err != nil {
				t.Fatal(err)
			}
			if gone := events == 0 && roomRows == 0; gone != purged {
				t.Fatalf("events=%d room rows=%d, want purged=%v", events, roomRows, purged)
			}
		})
	}
}
