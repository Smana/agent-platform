// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Smana/agent-platform/internal/envelope"
)

const room = "3kq7x2ma"

func open(t *testing.T) (*Store, string, string, string) {
	t.Helper()
	_, broker, retention, super := testDB(t)
	s, err := Open(context.Background(), broker)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if _, err := s.EnsureRoom(context.Background(), NewRoom{ID: room, Driver: "system:factory", Retention: 90 * 24 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	return s, broker, retention, super
}

func draft(client string, n int64) envelope.Draft {
	return draftIn(room, client, n)
}

func draftIn(roomID, client string, n int64) envelope.Draft {
	return envelope.Draft{RoomID: roomID, RunID: "7f3cq2xz",
		Actor: envelope.Actor{Kind: envelope.ActorAgent, ID: "agent:7f3cq2xz", Role: "implementer"},
		Type:  envelope.Message, Origin: envelope.OriginHarness, OriginClient: client, OriginSeq: n,
		Payload: envelope.Must(envelope.MessagePayload{Kind: envelope.KindChat, Text: fmt.Sprint("m", n), Delivery: envelope.DeliveryNone})}
}

// gapless reports whether the room's seqs are exactly 1..last_seq.
func gapless(t *testing.T, s *Store, roomID string) bool {
	t.Helper()
	var ok bool
	if err := s.pool.QueryRow(context.Background(), `SELECT coalesce(max(e.seq), 0) = count(e.*) AND count(e.*) = r.last_seq
		FROM rooms r LEFT JOIN events e USING (room_id) WHERE r.room_id = $1 GROUP BY r.last_seq`, roomID).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

// SC-1: gapless under concurrent writers.
func TestAppendIsGapless(t *testing.T) {
	s, _, _, _ := open(t)
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := int64(1); i <= 50; i++ {
				if _, _, err := s.Append(context.Background(), draft(fmt.Sprint("agent:w", w), i)); err != nil {
					t.Error(err)
				}
			}
		}(w)
	}
	wg.Wait()
	if !gapless(t, s, room) {
		t.Fatal("the log has a gap")
	}
}

func TestAppendIsIdempotentAndStaysGapless(t *testing.T) {
	s, _, _, _ := open(t)
	first, dup, err := s.Append(context.Background(), draft("agent:7f3cq2xz", 1))
	if err != nil || dup {
		t.Fatalf("first append: dup=%v err=%v", dup, err)
	}
	again, dup, err := s.Append(context.Background(), draft("agent:7f3cq2xz", 1))
	if err != nil || !dup || again.Seq != first.Seq {
		t.Fatalf("replay: seq=%d dup=%v err=%v", again.Seq, dup, err)
	}
	next, _, _ := s.Append(context.Background(), draft("agent:7f3cq2xz", 2))
	if next.Seq != first.Seq+1 {
		t.Fatalf("a duplicate left a gap: %d after %d", next.Seq, first.Seq)
	}
	if c, _ := s.Cursor(context.Background(), room, "agent:7f3cq2xz"); c != 2 {
		t.Fatalf("cursor = %d", c)
	}
}

func TestSealedRoomRefusesAppends(t *testing.T) {
	s, _, _, _ := open(t)
	if err := s.CloseRoom(context.Background(), room, "owner closed it"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Append(context.Background(), draft("agent:x", 1)); !errors.Is(err, ErrSealed) {
		t.Fatalf("want ErrSealed, got %v", err)
	}
	evs, err := s.Range(context.Background(), room, 0, 10)
	if err != nil || len(evs) == 0 {
		t.Fatalf("range: %d events, %v", len(evs), err)
	}
	if last := evs[len(evs)-1]; last.Type != envelope.StateChanged {
		t.Fatalf("closing appends a final state_changed, got %s", last.Type)
	}
	if err := s.CloseRoom(context.Background(), room, "again"); err != nil {
		t.Fatalf("closing twice is a no-op, got %v", err)
	}
}

func TestLimitSealsTheRoom(t *testing.T) {
	s, _, _, _ := open(t)
	s.MaxEvents = 3
	var sealing envelope.Event
	for i := int64(1); i <= 2; i++ {
		ev, _, err := s.Append(context.Background(), draft("agent:x", i))
		if err != nil {
			t.Fatal(err)
		}
		sealing = ev
	}
	st, err := s.Room(context.Background(), room)
	if err != nil || !st.Sealed || st.LastSeq != 3 || st.ClosedAt == nil {
		t.Fatalf("want sealed at seq 3 with a limit event, got %+v, %v", st, err)
	}
	// A retry of the append that sealed the room is a duplicate, not a refusal.
	replay, dup, err := s.Append(context.Background(), draft("agent:x", 2))
	if err != nil || !dup || replay.Seq != sealing.Seq {
		t.Fatalf("replay of the sealing append: seq=%d dup=%v err=%v", replay.Seq, dup, err)
	}
	if _, _, err := s.Append(context.Background(), draft("agent:x", 3)); !errors.Is(err, ErrSealed) {
		t.Fatalf("a new append after the seal: want ErrSealed, got %v", err)
	}
}

func TestOversizePayloadIsStubbed(t *testing.T) {
	s, _, _, _ := open(t)
	d := draft("agent:x", 1)
	d.Payload = envelope.Must(map[string]string{"output": string(make([]byte, envelope.MaxPayload))})
	ev, _, err := s.Append(context.Background(), d)
	if err != nil || len(ev.Payload) > 100 {
		t.Fatalf("oversize not stubbed: %d bytes, %v", len(ev.Payload), err)
	}
}

// Appends racing a close either land before the closing event or get ErrSealed:
// the log stays gapless and the close is its last event.
func TestAppendRacesCloseRoom(t *testing.T) {
	s, _, _, _ := open(t)
	ctx := context.Background()
	started := make(chan struct{})
	var once sync.Once
	var wg sync.WaitGroup
	errs := make(chan error, 4*40)
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := int64(1); i <= 40; i++ {
				_, _, err := s.Append(ctx, draft(fmt.Sprint("agent:w", w), i))
				errs <- err
				if i == 5 {
					once.Do(func() { close(started) })
				}
			}
		}(w)
	}
	<-started
	if err := s.CloseRoom(ctx, room, "closed mid-stream"); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	close(errs)
	var refused int
	for err := range errs {
		switch {
		case err == nil:
		case errors.Is(err, ErrSealed):
			refused++
		default:
			t.Errorf("an append racing the close: %v", err)
		}
	}
	if refused == 0 {
		t.Fatal("no append ran after the close: the race was not exercised")
	}
	if !gapless(t, s, room) {
		t.Fatal("the log has a gap")
	}
	var typ, kind string
	if err := s.pool.QueryRow(ctx, `SELECT type, payload->>'kind' FROM events WHERE room_id = $1
		ORDER BY seq DESC LIMIT 1`, room).Scan(&typ, &kind); err != nil {
		t.Fatal(err)
	}
	if typ != string(envelope.StateChanged) || kind != "room_phase" {
		t.Fatalf("the last event is %s/%s, want the close", typ, kind)
	}
}

func TestEnsureRoomRefusesNonPositiveRetention(t *testing.T) {
	s, _, _, _ := open(t)
	for _, retention := range []time.Duration{0, -time.Hour} {
		t.Run(retention.String(), func(t *testing.T) {
			created, err := s.EnsureRoom(context.Background(), NewRoom{ID: "zzzzzzzz", Driver: "system:factory", Retention: retention})
			if created || !errors.Is(err, ErrInvalidRetention) {
				t.Fatalf("created=%v err=%v, want ErrInvalidRetention", created, err)
			}
		})
	}
}

// Every pooled session is bounded, so no statement, lock wait or abandoned
// transaction can hold a room's row lock indefinitely.
func TestPoolBoundsEverySession(t *testing.T) {
	s, _, _, _ := open(t)
	for _, tc := range []struct{ param, want string }{
		{"statement_timeout", "15s"},
		{"lock_timeout", "5s"},
		{"idle_in_transaction_session_timeout", "30s"},
	} {
		t.Run(tc.param, func(t *testing.T) {
			var got string
			if err := s.pool.QueryRow(context.Background(), "SHOW "+tc.param).Scan(&got); err != nil || got != tc.want {
				t.Fatalf("%s = %q, %v; want %q", tc.param, got, err, tc.want)
			}
		})
	}
}

// Until phase 5 migrates the approvals table, nothing is pending; once it exists,
// only this room's pending rows count.
func TestPendingApprovals(t *testing.T) {
	s, _, _, super := open(t)
	ctx := context.Background()
	if n, err := s.PendingApprovals(ctx, room); err != nil || n != 0 {
		t.Fatalf("without the table: %d, %v; want 0", n, err)
	}
	// A stand-in with the columns the count reads, shaped like phase 5's table.
	exec(t, super, `CREATE TABLE approvals (approval_id text PRIMARY KEY, room_id text NOT NULL, state text NOT NULL);
		GRANT SELECT ON approvals TO rooms_broker;
		INSERT INTO approvals VALUES ('a1', '`+room+`', 'pending'), ('a2', '`+room+`', 'approved'),
			('a3', 'otherroo', 'pending'), ('a4', '`+room+`', 'pending')`)
	if n, err := s.PendingApprovals(ctx, room); err != nil || n != 2 {
		t.Fatalf("with the table: %d, %v; want 2", n, err)
	}
}
