package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

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
	return envelope.Draft{RoomID: room, RunID: "7f3cq2xz",
		Actor: envelope.Actor{Kind: envelope.ActorAgent, ID: "agent:7f3cq2xz", Role: "implementer"},
		Type:  envelope.Message, Origin: envelope.OriginHarness, OriginClient: client, OriginSeq: n,
		Payload: envelope.Must(envelope.MessagePayload{Kind: envelope.KindChat, Text: fmt.Sprint("m", n), Delivery: envelope.DeliveryNone})}
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
	var gapless bool
	if err := s.pool.QueryRow(context.Background(),
		`SELECT max(seq) = count(*) FROM events WHERE room_id = $1`, room).Scan(&gapless); err != nil || !gapless {
		t.Fatalf("gapless = %v, err = %v", gapless, err)
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
		t.Fatalf("a rolled-back duplicate left a gap: %d after %d", next.Seq, first.Seq)
	}
	if c, _ := s.Cursor(context.Background(), room, "agent:7f3cq2xz"); c != 2 {
		t.Fatalf("cursor = %d", c)
	}
}

// SC-10: the broker role cannot rewrite history.
func TestBrokerRoleIsAppendOnly(t *testing.T) {
	s, broker, _, _ := open(t)
	_, _, _ = s.Append(context.Background(), draft("agent:7f3cq2xz", 1))
	for _, sql := range []string{`UPDATE events SET payload = '{}'`, `DELETE FROM events`} {
		conn, _ := Open(context.Background(), broker)
		_, err := conn.pool.Exec(context.Background(), sql)
		conn.Close()
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || pg.Code != "42501" {
			t.Fatalf("%s: want permission denied (42501), got %v", sql, err)
		}
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
	evs, _ := s.Range(context.Background(), room, 0, 10)
	if last := evs[len(evs)-1]; last.Type != envelope.StateChanged {
		t.Fatalf("closing appends a final state_changed, got %s", last.Type)
	}
}

func TestLimitSealsTheRoom(t *testing.T) {
	s, _, _, _ := open(t)
	s.MaxEvents = 3
	for i := int64(1); i <= 2; i++ {
		if _, _, err := s.Append(context.Background(), draft("agent:x", i)); err != nil {
			t.Fatal(err)
		}
	}
	st, _ := s.Room(context.Background(), room)
	if !st.Sealed || st.LastSeq != 3 {
		t.Fatalf("want sealed at seq 3 with a limit event, got %+v", st)
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

func TestRetentionDeletesOnlyExpiredClosedRooms(t *testing.T) {
	s, _, retention, super := open(t)
	_, _, _ = s.Append(context.Background(), draft("agent:x", 1))
	r, _ := Open(context.Background(), retention)
	defer r.Close()
	tag, _ := r.pool.Exec(context.Background(), `DELETE FROM events`)
	if tag.RowsAffected() != 0 {
		t.Fatalf("an open room's events were deletable")
	}
	_ = s.CloseRoom(context.Background(), room, "done")
	exec(t, super, `UPDATE rooms SET closed_at = now() - interval '91 days'`)
	tag, _ = r.pool.Exec(context.Background(), `DELETE FROM events`)
	if tag.RowsAffected() == 0 {
		t.Fatalf("an expired room's events survived")
	}
}
