// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Smana/agent-platform/internal/envelope"
)

func humanDraft(who string, n int64) envelope.Draft {
	return envelope.Draft{RoomID: room, Actor: envelope.Actor{Kind: envelope.ActorHuman, ID: who},
		Origin: envelope.OriginClient, OriginClient: who + ":s1", OriginSeq: n, Payload: []byte(`{}`), Type: envelope.Driver}
}

// SC-3 offline: fencing. Two replicas racing the same give: one wins, one is stale.
func TestDriverChangeIsFenced(t *testing.T) {
	s, _, _, _ := open(t)
	ev, err := s.ChangeDriver(context.Background(), room, 0, "human:alice", "requested", humanDraft("human:alice", 1))
	if err != nil || ev.Type != envelope.Driver {
		t.Fatal(ev, err)
	}
	if _, err := s.ChangeDriver(context.Background(), room, 0, "human:bob", "requested", humanDraft("human:bob", 1)); !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("a stale epoch must lose: %v", err)
	}
	st, _ := s.Room(context.Background(), room)
	if st.Driver != "human:alice" || st.DriverEpoch != 1 || st.FallbackDriver != "system:factory" {
		t.Fatalf("%+v", st)
	}
	if string(ev.Payload) != `{"from":"system:factory","to":"human:alice","epoch":1,"reason":"requested"}` {
		t.Fatalf("payload %s", ev.Payload)
	}
}

// A retried change returns the event it stored and moves the token no further.
func TestDriverChangeReplayIsIdempotent(t *testing.T) {
	ctx := t.Context()
	s, _, _, _ := open(t)
	first, err := s.ChangeDriver(ctx, room, 0, "human:alice", "requested", humanDraft("human:alice", 1))
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.ChangeDriver(ctx, room, 0, "human:alice", "requested", humanDraft("human:alice", 1))
	if err != nil || again.Seq != first.Seq || again.ID != first.ID {
		t.Fatalf("replay = %+v, %v; want the stored event %+v", again, err, first)
	}
	if st, _ := s.Room(ctx, room); st.DriverEpoch != 1 || st.LastSeq != 1 {
		t.Fatalf("a replay moved the token or appended: %+v", st)
	}
}

func TestDriverChangeRefusals(t *testing.T) {
	ctx := t.Context()
	s, _, _, _ := open(t)
	if _, err := s.ChangeDriver(ctx, "nosuchrm", 0, "human:alice", "requested", humanDraft("human:alice", 1)); !errors.Is(err, ErrNoRoom) {
		t.Fatalf("no room: %v", err)
	}
	if err := s.CloseRoom(ctx, room, "done"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ChangeDriver(ctx, room, 0, "human:alice", "requested", humanDraft("human:alice", 1)); !errors.Is(err, ErrSealed) {
		t.Fatalf("a sealed room's token stays: %v", err)
	}
	bad := humanDraft("human:alice", 2)
	bad.OriginClient = ""
	if _, err := s.ChangeDriver(ctx, room, 0, "human:alice", "requested", bad); err == nil {
		t.Fatal("an invalid draft must be refused")
	}
}

// Review M4: a change names a principal other than the holder. Review M2: a key
// already stored for another event type is a conflict, not a change.
func TestDriverChangeRefusesBadTargetsAndForeignKeys(t *testing.T) {
	ctx := t.Context()
	s, _, _, _ := open(t)
	for _, to := range []string{"", "alice", "human:", "robot:x", "system:factory"} {
		if _, err := s.ChangeDriver(ctx, room, 0, to, "given", humanDraft("human:alice", 1)); !errors.Is(err, ErrInvalidDriver) {
			t.Fatalf("to %q: %v", to, err)
		}
	}
	if _, _, err := s.Append(ctx, draftIn(room, "human:zed:s1", 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ChangeDriver(ctx, room, 0, "human:zed", "requested", humanDraft("human:zed", 1)); !errors.Is(err, ErrKeyConflict) {
		t.Fatalf("a message's key replayed as a driver change: %v", err)
	}
	if st, _ := s.Room(ctx, room); st.DriverEpoch != 0 || st.LastSeq != 1 {
		t.Fatalf("a refused change moved the token or appended: %+v", st)
	}
	// Callers need not pass a payload: the store writes it.
	d := humanDraft("human:alice", 1)
	d.Payload = nil
	if ev, err := s.ChangeDriver(ctx, room, 0, "human:alice", "requested", d); err != nil || ev.Type != envelope.Driver {
		t.Fatalf("%+v, %v", ev, err)
	}
}

func queuedDraft(n int64, text string) envelope.Draft {
	return envelope.Draft{RoomID: room, Actor: envelope.Actor{Kind: envelope.ActorHuman, ID: "human:alice"},
		Type: envelope.Message, Origin: envelope.OriginClient, OriginClient: "human:alice:s1", OriginSeq: n,
		Payload: envelope.Must(envelope.MessagePayload{Kind: envelope.KindChat, Text: text, Delivery: envelope.DeliveryQueued})}
}

// Review I2: the queue is FIFO, and a removed message leaves the others in order.
func TestQueueKeepsFIFOOrder(t *testing.T) {
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
	if err := s.SetQueued(ctx, room, refs[1], "queued", "removed", ""); err != nil {
		t.Fatal(err)
	}
	got, err := s.Queue(ctx, room)
	if err != nil || len(got) != 2 || got[0].Ref != refs[0] || got[0].Text != "first" || got[1].Ref != refs[2] || got[1].Text != "third" {
		t.Fatalf("queue = %+v, %v; want [first, third]", got, err)
	}
}

// Review M2: a key already stored for another event type is not an enqueue.
func TestEnqueueRefusesAForeignKey(t *testing.T) {
	ctx := t.Context()
	s, _, _, _ := open(t)
	if _, err := s.ChangeDriver(ctx, room, 0, "human:alice", "requested", humanDraft("human:alice", 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enqueue(ctx, queuedDraft(1, "late"), "human:alice", "late"); !errors.Is(err, ErrKeyConflict) {
		t.Fatalf("a driver change's key replayed as an enqueue: %v", err)
	}
	if got, err := s.Queue(ctx, room); err != nil || len(got) != 0 {
		t.Fatalf("%+v, %v", got, err)
	}
}

// A human-to-human give keeps the system fallback; a system holder becomes it.
func TestFallbackIsTheLastSystemHolder(t *testing.T) {
	ctx := t.Context()
	s, _, _, _ := open(t)
	for i, to := range []string{"human:alice", "human:bob", "system:reviewer", "human:carol"} {
		if _, err := s.ChangeDriver(ctx, room, int64(i), to, "given", humanDraft("human:alice", int64(i+1))); err != nil {
			t.Fatal(err)
		}
	}
	if st, _ := s.Room(ctx, room); st.Driver != "human:carol" || st.DriverEpoch != 4 || st.FallbackDriver != "system:reviewer" {
		t.Fatalf("%+v", st)
	}
}

func TestLapsedHumanDriverIsListed(t *testing.T) {
	s, _, _, super := open(t)
	_, _ = s.ChangeDriver(context.Background(), room, 0, "human:alice", "requested", humanDraft("human:alice", 1))
	if l, _ := s.LapsedDrivers(context.Background()); len(l) != 0 {
		t.Fatal("a fresh holder is not lapsed")
	}
	exec(t, super, `UPDATE rooms SET driver_seen_at = now() - interval '3 minutes'`)
	if l, _ := s.LapsedDrivers(context.Background()); len(l) != 1 || l[0].FallbackDriver != "system:factory" {
		t.Fatalf("%+v", l)
	}
}

// The heartbeat keeps a connected holder; only an action keeps an idle one; a
// non-holder's heartbeat keeps nobody.
func TestDriverSeen(t *testing.T) {
	ctx := t.Context()
	s, _, _, super := open(t)
	if _, err := s.ChangeDriver(ctx, room, 0, "human:alice", "requested", humanDraft("human:alice", 1)); err != nil {
		t.Fatal(err)
	}
	lapsed := func() bool {
		t.Helper()
		l, err := s.LapsedDrivers(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return len(l) == 1
	}
	exec(t, super, `UPDATE rooms SET driver_seen_at = now() - interval '3 minutes'`)
	if err := s.DriverSeen(ctx, room, "human:bob", true); err != nil || !lapsed() {
		t.Fatalf("a non-holder's heartbeat kept the holder: %v", err)
	}
	if err := s.DriverSeen(ctx, room, "human:alice", false); err != nil || lapsed() {
		t.Fatalf("the holder's heartbeat did not keep it: %v", err)
	}
	exec(t, super, `UPDATE rooms SET driver_acted_at = now() - interval '16 minutes'`)
	if err := s.DriverSeen(ctx, room, "human:alice", false); err != nil || !lapsed() {
		t.Fatalf("a heartbeat alone kept an idle holder: %v", err)
	}
	if err := s.DriverSeen(ctx, room, "human:alice", true); err != nil || lapsed() {
		t.Fatalf("an action did not keep the holder: %v", err)
	}
}

// Only human holders with a system fallback lapse: a system holder never does, nor
// a human who started the room and has nobody to fall back to.
func TestLapsedDriversScope(t *testing.T) {
	ctx := t.Context()
	s, _, _, super := open(t)
	if _, err := s.EnsureRoom(ctx, NewRoom{ID: "humanonl", Driver: "human:alice", Retention: 24 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	exec(t, super, `UPDATE rooms SET driver_seen_at = now() - interval '1 hour', driver_acted_at = now() - interval '1 hour'`)
	if l, err := s.LapsedDrivers(ctx); err != nil || len(l) != 0 {
		t.Fatalf("%+v, %v", l, err)
	}
}

func TestQueueIsFIFOAndStateful(t *testing.T) {
	s, _, _, _ := open(t)
	q := func(n int64, text string) envelope.Draft {
		return envelope.Draft{RoomID: room, Actor: envelope.Actor{Kind: envelope.ActorHuman, ID: "human:alice"},
			Type: envelope.Message, Origin: envelope.OriginClient, OriginClient: "human:alice:s1", OriginSeq: n,
			Payload: envelope.Must(envelope.MessagePayload{Kind: envelope.KindChat, Text: text, Delivery: envelope.DeliveryQueued})}
	}
	a, _ := s.Enqueue(context.Background(), q(1, "first"), "human:alice", "first")
	b, _ := s.Enqueue(context.Background(), q(2, "second"), "human:alice", "second")
	if err := s.SetQueued(context.Background(), room, a.Seq, "queued", "removed", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.SetQueued(context.Background(), room, a.Seq, "queued", "consumed", "7f3cq2xz"); !errors.Is(err, ErrNotQueued) {
		t.Fatalf("a removed message is not consumable: %v", err)
	}
	got, _ := s.Queue(context.Background(), room)
	if len(got) != 1 || got[0].Ref != b.Seq || got[0].Text != "second" {
		t.Fatalf("%+v", got)
	}
}

// A retried enqueue returns the stored event and adds no second queue row.
func TestEnqueueReplayIsIdempotent(t *testing.T) {
	ctx := t.Context()
	s, _, _, _ := open(t)
	d := envelope.Draft{RoomID: room, Actor: envelope.Actor{Kind: envelope.ActorHuman, ID: "human:alice"},
		Type: envelope.Message, Origin: envelope.OriginClient, OriginClient: "human:alice:s1", OriginSeq: 1,
		Payload: envelope.Must(envelope.MessagePayload{Kind: envelope.KindChat, Text: "once", Delivery: envelope.DeliveryQueued})}
	first, err := s.Enqueue(ctx, d, "human:alice", "once")
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.Enqueue(ctx, d, "human:alice", "once")
	if err != nil || again.Seq != first.Seq {
		t.Fatalf("replay = %+v, %v", again, err)
	}
	if got, err := s.Queue(ctx, room); err != nil || len(got) != 1 || got[0].Author != "human:alice" || got[0].State != "queued" {
		t.Fatalf("%+v, %v", got, err)
	}
	// A consumed message records the run whose brief took it; a later move keeps it.
	if err := s.SetQueued(ctx, room, first.Seq, "queued", "consumed", "7f3cq2xz"); err != nil {
		t.Fatal(err)
	}
	var run string
	if err := s.pool.QueryRow(ctx, `SELECT run_id FROM queue WHERE room_id = $1 AND ref = $2`, room, first.Seq).Scan(&run); err != nil || run != "7f3cq2xz" {
		t.Fatalf("run_id = %q, %v", run, err)
	}
}

// LastAck is the highest ref the run's bridge acknowledged, delivered or
// interrupted; another run's acks and other kinds do not count.
func TestLastAck(t *testing.T) {
	ctx := t.Context()
	s, _, _, _ := open(t)
	if n, err := s.LastAck(ctx, room, "7f3cq2xz"); err != nil || n != 0 {
		t.Fatalf("no ack yet: %d, %v", n, err)
	}
	for i, c := range []struct {
		run, kind string
		ref       any
	}{
		{"7f3cq2xz", "delivered", 3},
		{"7f3cq2xz", "interrupted", 5},
		{"7f3cq2xz", "harness_status", 9},
		{"aaaaaaaa", "delivered", 12},
		{"7f3cq2xz", "delivered", "99x"}, // not a seq: skipped, not a failed cast
		{"7f3cq2xz", "delivered", 1e30},
	} {
		d := draftIn(room, "agent:ack", int64(i+1))
		d.RunID, d.Type = c.run, envelope.StateChanged
		d.Payload = envelope.StatePayload(c.kind, map[string]any{"ref": c.ref})
		if _, _, err := s.Append(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	if n, err := s.LastAck(ctx, room, "7f3cq2xz"); err != nil || n != 5 {
		t.Fatalf("last ack = %d, %v; want 5", n, err)
	}
}
