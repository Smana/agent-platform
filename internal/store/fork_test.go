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
