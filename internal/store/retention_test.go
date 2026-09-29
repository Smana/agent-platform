// SPDX-License-Identifier: Apache-2.0

package store

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Smana/agent-platform/internal/envelope"
)

// OD-17: the retention job purges exactly the sealed rooms past their retention,
// events first, and the broker's role cannot purge at all.
func TestPurgeExpired(t *testing.T) {
	ctx := t.Context()
	s, _, retention, super := open(t)
	rooms := map[string]bool{ // room -> purged
		room:       false, // open(t)'s room: open, no events
		"openaaaa": false, // open, with events
		"unsealed": false, // a forged close date past retention, never sealed
		"freshaaa": false, // sealed within retention
		"expiredx": true,  // sealed, past retention
		"expiredy": true,  // sealed, past retention
	}
	for id := range rooms {
		if id == room {
			continue
		}
		if _, err := s.EnsureRoom(ctx, NewRoom{ID: id, Driver: "system:factory", Retention: 90 * 24 * time.Hour}); err != nil {
			t.Fatal(err)
		}
		for n := range int64(3) {
			if _, _, err := s.Append(ctx, draftIn(id, "agent:x", n+1)); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, id := range []string{"freshaaa", "expiredx", "expiredy"} {
		if err := s.CloseRoom(ctx, id, "done"); err != nil {
			t.Fatal(err)
		}
	}
	forge(t, super, `UPDATE rooms SET closed_at = now() - interval '91 days' WHERE room_id IN ('unsealed', 'expiredx', 'expiredy')`)

	if _, _, err := s.PurgeExpired(ctx); err == nil {
		t.Fatal("the broker's role must not be able to purge")
	}
	r, err := Open(ctx, retention)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	purgedRooms, purgedEvents, err := r.PurgeExpired(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Each expired room held 3 events and its seal.
	if purgedRooms != 2 || purgedEvents != 8 {
		t.Fatalf("purged %d rooms and %d events, want 2 and 8", purgedRooms, purgedEvents)
	}
	for id, purged := range rooms {
		t.Run(id, func(t *testing.T) {
			_, err := s.Room(ctx, id)
			if gone := err != nil; gone != purged {
				t.Fatalf("room gone=%v (%v), want purged=%v", gone, err, purged)
			}
		})
	}
	if again, _, err := r.PurgeExpired(ctx); err != nil || again != 0 {
		t.Fatalf("a second run purged %d rooms (%v), want 0", again, err)
	}
}

// Review M2: the sweep reads only the broker's own scopes through a partial index,
// not every event of every open room.
func TestUnfinishedUsesItsIndex(t *testing.T) {
	ctx := t.Context()
	s, _, _, super := open(t)
	// A long log of harness events, forged in bulk, and one broker scope.
	forge(t, super, `INSERT INTO events (room_id, seq, id, actor_kind, actor_id, type, origin, origin_client,
		origin_seq, ts, payload)
		SELECT '`+room+`', g, 'id' || g, 'agent', 'agent:7f3cq2xz', 'message', 'harness', 'agent:7f3cq2xz', g, now(), '{}'
		FROM generate_series(1, 20000) g;
		INSERT INTO events (room_id, seq, id, run_id, actor_kind, actor_id, type, origin, origin_client,
		origin_seq, ts, payload)
		VALUES ('`+room+`', 20001, 'idj', '7f3cq2xz', 'system', 'system:room-broker', 'participant', 'broker',
		'broker:run:7f3cq2xz', 1, now(), '{}');
		ANALYZE events;`)
	var plan []string
	rows, err := s.pool.Query(ctx, `EXPLAIN `+unfinishedSQL, "broker:run:", int64(1), int64(4))
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	text := strings.Join(plan, "\n")

	// Both sides: the scopes it lists (e) and the closing event it looks for (c).
	if !strings.Contains(text, "using events_broker_scopes on events e") ||
		!strings.Contains(text, "using events_broker_scopes on events c") || strings.Contains(text, "Seq Scan on events") {
		t.Fatalf("the sweep does not read through events_broker_scopes:\n%s", text)
	}
	evs, err := s.Unfinished(ctx, "broker:run:", 1, 4)
	if err != nil || len(evs) != 1 || evs[0].RunID != "7f3cq2xz" {
		t.Fatalf("Unfinished = %v, %v; want the one open broker scope", evs, err)
	}
}

// The sweep's query: the first event of every scope that wrote its opening seq
// and not its closing one, in rooms still open.
func TestUnfinished(t *testing.T) {
	ctx := t.Context()
	s, _, _, _ := open(t)
	if _, err := s.EnsureRoom(ctx, NewRoom{ID: "sealedaa", Driver: "system:factory", Retention: 24 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	put := func(roomID, client, runID string, n int64, origin envelope.Origin) {
		t.Helper()
		d := draftIn(roomID, client, n)
		d.RunID, d.Origin = runID, origin
		if origin == envelope.OriginBroker {
			d.Actor = envelope.Actor{Kind: envelope.ActorSystem, ID: "system:room-broker"}
		}
		if _, _, err := s.Append(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	broker, harness := envelope.OriginBroker, envelope.OriginHarness
	put(room, "broker:run:aaaaaaaa", "aaaaaaaa", 1, broker) // opened, never closed: listed
	put(room, "broker:run:bbbbbbbb", "bbbbbbbb", 1, broker) // opened and closed
	put(room, "broker:run:bbbbbbbb", "bbbbbbbb", 4, broker)
	put(room, "broker:busy:cccccccc", "cccccccc", 1, broker)      // another prefix
	put(room, "broker:run:dddddddd", "dddddddd", 2, broker)       // never opened
	put("sealedaa", "broker:run:eeeeeeee", "eeeeeeee", 1, broker) // in a sealed room
	put(room, "broker:run:ffffffff", "ffffffff", 1, harness)      // not the broker's own scope
	if err := s.CloseRoom(ctx, "sealedaa", "done"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name        string
		prefix      string
		first, last int64
		want        []string
	}{
		{"opened and not closed, the broker's, in open rooms only", "broker:run:", 1, 4, []string{"aaaaaaaa"}},
		{"the prefix is literal, not a pattern", "broker:run%", 1, 4, nil},
		{"another scope", "broker:busy:", 1, 4, []string{"cccccccc"}},
		{"never a scope another origin wrote", "agent:", 1, 4, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			evs, err := s.Unfinished(ctx, c.prefix, c.first, c.last)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, ev := range evs {
				if ev.RoomID != room || ev.Seq == 0 {
					t.Fatalf("event %+v is not a full event of %s", ev, room)
				}
				got = append(got, ev.RunID)
			}
			if !slices.Equal(got, c.want) {
				t.Fatalf("got %v want %v", got, c.want)
			}
		})
	}
}
