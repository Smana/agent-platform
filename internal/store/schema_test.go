// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
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
)

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
	if err := s.CloseRoom(ctx, sealedRoom, "sealed for the test"); err != nil {
		t.Fatal(err)
	}

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
		{"a legitimate append by hand", handAppend, "", ""},
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
