// SPDX-License-Identifier: Apache-2.0

package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"maps"
	"testing"
	"time"

	"github.com/Smana/agent-platform/internal/envelope"
)

// SC-7, offline: the fork copies 1..N with their seq, as the broker's own role,
// and leaves the source room as it was.
func TestForkCopiesThePrefixExactly(t *testing.T) {
	s, _, _, _ := open(t)
	ctx := context.Background()
	for i := int64(1); i <= 5; i++ {
		if _, _, err := s.Append(ctx, draft("agent:x", i)); err != nil {
			t.Fatal(err)
		}
	}
	by := envelope.Draft{Actor: envelope.Actor{Kind: envelope.ActorHuman, ID: "human:bob"}, Origin: envelope.OriginClient,
		OriginClient: "human:bob:s1", OriginSeq: 1, Payload: []byte(`{}`), Type: envelope.StateChanged}
	dst := NewRoom{ID: "abcdefgh", Driver: "human:bob", Retention: 90 * 24 * time.Hour}
	ev, err := s.Fork(ctx, room, 3, dst, by, map[string]any{"note": "try the v2 API", "commit": "4be1c9d"})
	if err != nil || ev.Seq != 4 || ev.RoomID != "abcdefgh" || ev.Actor.ID != "human:bob" {
		t.Fatal(ev, err)
	}
	src, _ := s.Range(ctx, room, 0, 10)
	got, _ := s.Range(ctx, "abcdefgh", 0, 10)
	if len(got) != 4 {
		t.Fatalf("%d events in the fork, want 3 copied and forked_from", len(got))
	}
	for i := 0; i < 3; i++ {
		a, b := sha256.Sum256(src[i].Payload), sha256.Sum256(got[i].Payload)
		if src[i].Seq != got[i].Seq || src[i].ID != got[i].ID || !bytes.Equal(a[:], b[:]) {
			t.Fatalf("seq %d differs", src[i].Seq)
		}
	}
	var p map[string]any
	want := map[string]any{"kind": "forked_from", "room": room, "seq": 3.0, "note": "try the v2 API", "commit": "4be1c9d"}
	if err := json.Unmarshal(got[3].Payload, &p); err != nil || got[3].Type != envelope.StateChanged || !maps.Equal(p, want) {
		t.Fatalf("%s", got[3].Payload)
	}
	if !gapless(t, s, "abcdefgh") {
		t.Fatal("the fork's seqs are not 1..last_seq")
	}
	st, err := s.Room(ctx, "abcdefgh")
	if err != nil || st.LastSeq != 4 || st.Driver != "human:bob" || st.FallbackDriver != "" || st.Sealed {
		t.Fatalf("%+v %v", st, err)
	}
	if st, _ := s.Room(ctx, room); st.LastSeq != 5 {
		t.Fatal("the source room is untouched")
	}
	// The copied keys come along: the Room controller's seq-1 Open, or any replay
	// of a copied write, deduplicates instead of appending twice.
	if _, dup, err := s.Append(ctx, draftIn("abcdefgh", "agent:x", 1)); err != nil || !dup {
		t.Fatalf("a copied key: dup %v, err %v", dup, err)
	}
}

func TestForkRefusals(t *testing.T) {
	s, _, _, _ := open(t)
	ctx := context.Background()
	for i := int64(1); i <= 2; i++ {
		if _, _, err := s.Append(ctx, draft("agent:x", i)); err != nil {
			t.Fatal(err)
		}
	}
	by := envelope.Draft{Actor: envelope.Actor{Kind: envelope.ActorHuman, ID: "human:bob"}, Origin: envelope.OriginClient,
		OriginClient: "human:bob:s1", OriginSeq: 1, Payload: []byte(`{}`), Type: envelope.StateChanged}
	nr := func(id string) NewRoom { return NewRoom{ID: id, Driver: "human:bob", Retention: 24 * time.Hour} }
	cases := []struct {
		name string
		src  string
		upTo int64
		r    NewRoom
		want error
	}{
		{"a seq past the end", room, 3, nr("bbbbbbbb"), ErrBadSeq},
		{"seq 0", room, 0, nr("bbbbbbbb"), ErrBadSeq},
		{"no such room", "zzzzzzzz", 1, nr("bbbbbbbb"), ErrNoRoom},
		{"a retention under a day", room, 1, NewRoom{ID: "bbbbbbbb", Driver: "human:bob", Retention: time.Hour}, ErrInvalidRetention},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := s.Fork(ctx, c.src, c.upTo, c.r, by, nil); !errors.Is(err, c.want) {
				t.Fatalf("got %v, want %v", err, c.want)
			}
			if _, err := s.Room(ctx, "bbbbbbbb"); !errors.Is(err, ErrNoRoom) {
				t.Fatal("a refused fork leaves no room behind")
			}
		})
	}
	// A closed room is still forked: the fork writes only the new room.
	if err := s.CloseRoom(ctx, room, "done"); err != nil {
		t.Fatal(err)
	}
	if ev, err := s.Fork(ctx, room, 3, nr("cccccccc"), by, nil); err != nil || ev.Seq != 4 {
		t.Fatalf("fork of a closed room through its seal event: %v %v", ev, err)
	}
	if st, _ := s.Room(ctx, "cccccccc"); st.Sealed {
		t.Fatal("copying the source's seal event does not seal the fork")
	}
}

// A fork is one transaction inside the act's 15 s, and the copy is new bytes
// under a fresh retention: a prefix over either cap is refused before it commits.
func TestForkRefusesAPrefixOverTheCaps(t *testing.T) {
	s, _, _, _ := open(t)
	ctx := context.Background()
	for i := int64(1); i <= 3; i++ {
		if _, _, err := s.Append(ctx, draft("agent:x", i)); err != nil {
			t.Fatal(err)
		}
	}
	by := envelope.Draft{Actor: envelope.Actor{Kind: envelope.ActorHuman, ID: "human:bob"}, Origin: envelope.OriginClient,
		OriginClient: "human:bob:s1", OriginSeq: 1, Payload: []byte(`{}`), Type: envelope.StateChanged}
	nr := NewRoom{ID: "bbbbbbbb", Driver: "human:bob", Retention: 24 * time.Hour}
	if s.MaxForkEvents != 5000 || s.MaxForkBytes != 32<<20 {
		t.Fatalf("caps %d events, %d bytes", s.MaxForkEvents, s.MaxForkBytes)
	}
	for _, c := range []struct {
		name          string
		events, bytes int64
	}{{"over the event cap", 2, 1 << 20}, {"over the byte cap", 10, 64}} {
		t.Run(c.name, func(t *testing.T) {
			s.MaxForkEvents, s.MaxForkBytes = c.events, c.bytes
			if _, err := s.Fork(ctx, room, 3, nr, by, nil); !errors.Is(err, ErrForkTooLarge) {
				t.Fatalf("got %v, want ErrForkTooLarge", err)
			}
			if _, err := s.Room(ctx, "bbbbbbbb"); !errors.Is(err, ErrNoRoom) {
				t.Fatal("a refused fork leaves no room behind")
			}
		})
	}
	s.MaxForkEvents, s.MaxForkBytes = 3, 1<<20
	if _, err := s.Fork(ctx, room, 3, nr, by, nil); err != nil {
		t.Fatalf("a prefix at the caps: %v", err)
	}
}

// Review focus 3: a fork carries the messages still queued at its point, and the
// source's later consume does not reach the fork's copy.
func TestAForkCarriesTheQueueAtItsPoint(t *testing.T) {
	ctx := t.Context()
	s, _, _, _ := open(t)
	q, err := s.Enqueue(ctx, queuedDraft(1, "address L42"), "human:alice", "address L42")
	if err != nil {
		t.Fatal(err)
	}
	later, err := s.Enqueue(ctx, queuedDraft(2, "after the point"), "human:alice", "after the point")
	if err != nil {
		t.Fatal(err)
	}
	by := envelope.Draft{Actor: envelope.Actor{Kind: envelope.ActorHuman, ID: "human:bob"}, Origin: envelope.OriginClient,
		OriginClient: "human:bob:s1", OriginSeq: 1, Payload: []byte(`{}`), Type: envelope.StateChanged}
	dst := NewRoom{ID: "abcdefgh", Driver: "human:bob", Retention: 90 * 24 * time.Hour}
	if _, err := s.Fork(ctx, room, q.Seq, dst, by, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.SetQueued(ctx, room, q.Seq, "consumed", "cf4ato2x"); err != nil { // the source consumes after the fork point
		t.Fatal(err)
	}
	got, err := s.Queue(ctx, "abcdefgh")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Ref != q.Seq || got[0].Author != "human:alice" || got[0].Text != "address L42" {
		t.Fatalf("fork queue %+v: want only the message queued at the fork point (%d, not %d)", got, q.Seq, later.Seq)
	}
}

// A message that left the source's queue after the fork point, by a logged move,
// is still queued at that point: the fork carries it.
func TestAForkCarriesWhatLeftTheQueueAfterItsPoint(t *testing.T) {
	alice := envelope.Draft{RoomID: room, Actor: envelope.Actor{Kind: envelope.ActorHuman, ID: "human:alice"},
		Origin: envelope.OriginClient, OriginClient: "human:alice:s1", OriginSeq: 50}
	for _, c := range []struct {
		name  string
		leave func(t *testing.T, s *Store, ref int64)
	}{
		{"promoted", func(t *testing.T, s *Store, ref int64) {
			ctx := t.Context()
			if _, _, err := s.ChangeDriver(ctx, room, 0, "human:own", "given", driverDraft(1)); err != nil {
				t.Fatal(err)
			}
			if _, err := s.PromoteQueued(ctx, room, ref, "human:own", 1, "7f3cq2xz", driverDraft(2)); err != nil {
				t.Fatal(err)
			}
		}},
		{"removed", func(t *testing.T, s *Store, ref int64) {
			if _, err := s.RemoveQueued(t.Context(), room, ref, alice); err != nil {
				t.Fatal(err)
			}
		}},
		{"consumed by a brief", func(t *testing.T, s *Store, ref int64) {
			if _, err := s.RecordRunRequest(t.Context(), runDraft(1), "aaaaaaaa", []int64{ref}, nil); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx := t.Context()
			s, _, _, _ := open(t)
			q, err := s.Enqueue(ctx, queuedDraft(1, "address L42"), "human:alice", "address L42")
			if err != nil {
				t.Fatal(err)
			}
			c.leave(t, s, q.Seq) // after the point, before the fork
			if got, err := s.Queue(ctx, room); err != nil || len(got) != 0 {
				t.Fatalf("the move left %+v queued in the source, %v", got, err)
			}
			by := envelope.Draft{Actor: envelope.Actor{Kind: envelope.ActorHuman, ID: "human:bob"}, Origin: envelope.OriginClient,
				OriginClient: "human:bob:s1", OriginSeq: 1, Payload: []byte(`{}`), Type: envelope.StateChanged}
			dst := NewRoom{ID: "abcdefgh", Driver: "human:bob", Retention: 90 * 24 * time.Hour}
			if _, err := s.Fork(ctx, room, q.Seq, dst, by, nil); err != nil {
				t.Fatal(err)
			}
			got, err := s.Queue(ctx, "abcdefgh")
			if err != nil || len(got) != 1 || got[0].Ref != q.Seq || got[0].State != "queued" {
				t.Fatalf("fork queue %+v, %v: want the message queued at the point", got, err)
			}
		})
	}
}

// A message that left the queue at or before the point is not queued there.
func TestAForkLeavesOutWhatLeftTheQueueBeforeItsPoint(t *testing.T) {
	ctx := t.Context()
	s, _, _, _ := open(t)
	q, err := s.Enqueue(ctx, queuedDraft(1, "gone"), "human:alice", "gone")
	if err != nil {
		t.Fatal(err)
	}
	rm, err := s.RemoveQueued(ctx, room, q.Seq, envelope.Draft{RoomID: room, Actor: envelope.Actor{Kind: envelope.ActorHuman, ID: "human:alice"},
		Origin: envelope.OriginClient, OriginClient: "human:alice:s1", OriginSeq: 50})
	if err != nil {
		t.Fatal(err)
	}
	by := envelope.Draft{Actor: envelope.Actor{Kind: envelope.ActorHuman, ID: "human:bob"}, Origin: envelope.OriginClient,
		OriginClient: "human:bob:s1", OriginSeq: 1, Payload: []byte(`{}`), Type: envelope.StateChanged}
	dst := NewRoom{ID: "abcdefgh", Driver: "human:bob", Retention: 90 * 24 * time.Hour}
	if _, err := s.Fork(ctx, room, rm.Seq, dst, by, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Queue(ctx, "abcdefgh"); err != nil || len(got) != 0 {
		t.Fatalf("fork queue %+v, %v: want empty", got, err)
	}
}
