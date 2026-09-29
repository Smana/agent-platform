// SPDX-License-Identifier: Apache-2.0

package store

import (
	"testing"
	"time"
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

// The sweep's query: the first event of every scope that wrote its opening seq
// and not its closing one, in rooms still open.
func TestUnfinished(t *testing.T) {
	ctx := t.Context()
	s, _, _, _ := open(t)
	if _, err := s.EnsureRoom(ctx, NewRoom{ID: "sealedaa", Driver: "system:factory", Retention: 24 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	put := func(roomID, client, runID string, n int64) {
		t.Helper()
		d := draftIn(roomID, client, n)
		d.RunID = runID
		if _, _, err := s.Append(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	put(room, "broker:run:aaaaaaaa", "aaaaaaaa", 1) // opened, never closed: listed
	put(room, "broker:run:bbbbbbbb", "bbbbbbbb", 1) // opened and closed
	put(room, "broker:run:bbbbbbbb", "bbbbbbbb", 4)
	put(room, "agent:cccccccc", "cccccccc", 1)            // another prefix
	put(room, "broker:run:dddddddd", "dddddddd", 2)       // never opened
	put("sealedaa", "broker:run:eeeeeeee", "eeeeeeee", 1) // in a sealed room
	if err := s.CloseRoom(ctx, "sealedaa", "done"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name        string
		prefix      string
		first, last int64
		want        []string
	}{
		{"opened and not closed, in open rooms only", "broker:run:", 1, 4, []string{"aaaaaaaa"}},
		{"the prefix is literal, not a pattern", "broker:run%", 1, 4, nil},
		{"another scope", "agent:", 1, 4, []string{"cccccccc"}},
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
			if len(got) != len(c.want) || (len(got) == 1 && got[0] != c.want[0]) {
				t.Fatalf("got %v want %v", got, c.want)
			}
		})
	}
}
