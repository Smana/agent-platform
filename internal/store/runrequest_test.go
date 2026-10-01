// SPDX-License-Identifier: Apache-2.0

package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Smana/agent-platform/internal/envelope"
)

func runDraft(n int64) envelope.Draft {
	return envelope.Draft{RoomID: room, Actor: envelope.Actor{Kind: envelope.ActorHuman, ID: "human:own"},
		Origin: envelope.OriginClient, OriginClient: "human:own:s1", OriginSeq: n}
}

// A run's request and the queued messages its brief quoted move together: the
// messages still queued are consumed by the run, one removed meanwhile stays
// removed, and the record names exactly the refs it consumed.
func TestRecordRunRequest(t *testing.T) {
	ctx := t.Context()
	s, _, _, _ := open(t)
	var refs []int64
	for i, text := range []string{"first", "second", "third"} {
		ev, err := s.Enqueue(ctx, queuedDraft(int64(i+1), text), "human:alice", text)
		if err != nil {
			t.Fatal(err)
		}
		refs = append(refs, ev.Seq)
	}
	if err := s.SetQueued(ctx, room, refs[1], "removed", ""); err != nil {
		t.Fatal(err)
	}
	fields := map[string]any{"role": "implementer", "via": "manifest", "baseRef": "main"}
	ev, err := s.RecordRunRequest(ctx, runDraft(1), "7f3cq2xz", refs, fields)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprintf(`{"baseRef":"main","consumed":[%d,%d],"kind":"run_requested","role":"implementer","runId":"7f3cq2xz","via":"manifest"}`,
		refs[0], refs[2])
	if ev.Type != envelope.StateChanged || string(ev.Payload) != want {
		t.Fatalf("%s %s, want %s", ev.Type, ev.Payload, want)
	}
	if q, err := s.Queue(ctx, room); err != nil || len(q) != 0 {
		t.Fatalf("still queued: %+v, %v", q, err)
	}
	var consumed int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM queue WHERE room_id = $1 AND state = 'consumed' AND run_id = '7f3cq2xz'`,
		room).Scan(&consumed); err != nil || consumed != 2 {
		t.Fatalf("consumed = %d, %v", consumed, err)
	}

	again, err := s.RecordRunRequest(ctx, runDraft(1), "7f3cq2xz", refs, fields)
	if err != nil || again.Seq != ev.Seq {
		t.Fatalf("a replay returns the record: %+v, %v", again, err)
	}
	if st, _ := s.Room(ctx, room); st.LastSeq != ev.Seq {
		t.Fatalf("a replay appended: last_seq %d", st.LastSeq)
	}
	// With nothing queued, the record names no ref.
	empty, err := s.RecordRunRequest(ctx, runDraft(2), "aaaaaaaa", nil, fields)
	if err != nil || !strings.Contains(string(empty.Payload), `"consumed":[]`) {
		t.Fatalf("%s, %v", empty.Payload, err)
	}
}

// A refused record moves no queued message.
func TestRecordRunRequestRefusals(t *testing.T) {
	ctx := t.Context()
	s, _, _, _ := open(t)
	q, err := s.Enqueue(ctx, queuedDraft(1, "keep"), "human:alice", "keep")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Append(ctx, draftIn(room, "human:own:s1", 9)); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name  string
		d     envelope.Draft
		run   string
		check func(error) bool
	}{
		{"a key stored for a message", runDraft(9), "7f3cq2xz", func(err error) bool { return errors.Is(err, ErrKeyConflict) }},
		{"a run that is no run id", runDraft(1), "NOT-A-RUN", func(err error) bool { return errors.Is(err, ErrBadMove) }},
		{"a draft with no origin", envelope.Draft{RoomID: room, Actor: envelope.Actor{Kind: envelope.ActorHuman, ID: "human:own"}, Origin: envelope.OriginClient},
			"7f3cq2xz", func(err error) bool { return err != nil }},
		{"no room", envelope.Draft{RoomID: "nosuchrm", Actor: envelope.Actor{Kind: envelope.ActorHuman, ID: "human:own"},
			Origin: envelope.OriginClient, OriginClient: "human:own:s1", OriginSeq: 1}, "7f3cq2xz", func(err error) bool { return errors.Is(err, ErrNoRoom) }},
	} {
		if _, err := s.RecordRunRequest(ctx, c.d, c.run, []int64{q.Seq}, map[string]any{"role": "implementer"}); !c.check(err) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	if err := s.CloseRoom(ctx, room, "done"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordRunRequest(ctx, runDraft(1), "7f3cq2xz", []int64{q.Seq}, nil); !errors.Is(err, ErrSealed) {
		t.Errorf("sealed: %v", err)
	}
	var state string
	if err := s.pool.QueryRow(ctx, `SELECT state FROM queue WHERE room_id = $1 AND ref = $2`, room, q.Seq).Scan(&state); err != nil || state != "queued" {
		t.Fatalf("a refused record moved the message: %q, %v", state, err)
	}
}

// Stored finds what a key already recorded, before an act with side effects
// outside the log (a run request) runs twice.
func TestStored(t *testing.T) {
	ctx := t.Context()
	s, _, _, _ := open(t)
	d := draftIn(room, "human:own:s1", 1)
	if _, dup, err := s.Stored(ctx, d); err != nil || dup {
		t.Fatalf("a fresh key: %v, %v", dup, err)
	}
	ev, _, err := s.Append(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if got, dup, err := s.Stored(ctx, d); err != nil || !dup || got.Seq != ev.Seq || got.Type != envelope.Message {
		t.Fatalf("%+v, %v, %v", got, dup, err)
	}
}

func leaseDraft(epoch int64) envelope.Draft {
	return envelope.Draft{RoomID: room, Actor: envelope.Actor{Kind: envelope.ActorSystem, ID: "system:room-broker"},
		Origin: envelope.OriginBroker, OriginClient: "broker:lease:" + room, OriginSeq: epoch + 1}
}

// The lease expires only while the holder is still lapsed under the row lock:
// a holder who came back between the sweep's read and its write keeps the token.
func TestExpireDriver(t *testing.T) {
	ctx := t.Context()
	s, _, _, super := open(t)
	if _, _, err := s.ChangeDriver(ctx, room, 0, "human:alice", "requested", humanDraft("human:alice", 1)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ExpireDriver(ctx, room, 1, "system:factory", leaseDraft(1)); !errors.Is(err, ErrNotLapsed) {
		t.Fatalf("a present holder lapsed: %v", err)
	}
	exec(t, super, `UPDATE rooms SET driver_seen_at = now() - interval '3 minutes'`)
	if _, _, err := s.ExpireDriver(ctx, room, 1, "system:other", leaseDraft(1)); !errors.Is(err, ErrNotLapsed) {
		t.Fatalf("a lapse goes to the fallback only: %v", err)
	}
	ev, dup, err := s.ExpireDriver(ctx, room, 1, "system:factory", leaseDraft(1))
	if err != nil || dup || string(ev.Payload) != `{"from":"human:alice","to":"system:factory","epoch":2,"reason":"lease_expired"}` {
		t.Fatalf("%s, %v, %v", ev.Payload, dup, err)
	}
	// Two overlapping leaders build the same key: the second is a replay (review 4.4 M5).
	if again, dup, err := s.ExpireDriver(ctx, room, 1, "system:factory", leaseDraft(1)); err != nil || !dup || again.Seq != ev.Seq {
		t.Fatalf("a replay returns the change: %+v, %v, %v", again, dup, err)
	}
	if st, _ := s.Room(ctx, room); st.Driver != "system:factory" || st.DriverEpoch != 2 {
		t.Fatalf("%+v", st)
	}
	// A system holder's lease never expires, however stale its heartbeat.
	if _, _, err := s.ChangeDriver(ctx, room, 2, "system:reviewer", "given", humanDraft("human:alice", 2)); err != nil {
		t.Fatal(err)
	}
	exec(t, super, `UPDATE rooms SET driver_seen_at = now() - interval '1 hour', driver_acted_at = now() - interval '1 hour'`)
	if _, _, err := s.ExpireDriver(ctx, room, 3, "system:factory", leaseDraft(3)); !errors.Is(err, ErrNotLapsed) {
		t.Fatalf("a system holder lapsed: %v", err)
	}
}
