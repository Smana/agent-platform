// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/Smana/agent-platform/internal/envelope"
)

// Review 4.1: a queue row's author is its event's actor, and its text the event's.
func TestEnqueueAuthorIsTheActor(t *testing.T) {
	ctx := t.Context()
	s, _, _, _ := open(t)
	if _, err := s.Enqueue(ctx, queuedDraft(1, "mine"), "human:bob", "mine"); !errors.Is(err, ErrNotAQueuedMessage) {
		t.Fatalf("another author: %v", err)
	}
	if _, err := s.Enqueue(ctx, queuedDraft(1, "mine"), "human:alice", "not the payload's"); !errors.Is(err, ErrNotAQueuedMessage) {
		t.Fatalf("another text: %v", err)
	}
	plain := queuedDraft(1, "mine")
	plain.Payload = envelope.Must(envelope.MessagePayload{Kind: envelope.KindChat, Text: "mine", Delivery: envelope.DeliveryNone})
	if _, err := s.Enqueue(ctx, plain, "human:alice", "mine"); !errors.Is(err, ErrNotAQueuedMessage) {
		t.Fatalf("a message not queued: %v", err)
	}
	if _, err := s.Enqueue(ctx, queuedDraft(1, "mine"), "human:alice", "mine"); err != nil {
		t.Fatal(err)
	}
	var matches bool
	if err := s.pool.QueryRow(ctx, `SELECT bool_and(q.author = e.actor_id AND q.text = e.payload->>'text') AND count(*) = 1
		FROM queue q JOIN events e ON e.room_id = q.room_id AND e.seq = q.ref`).Scan(&matches); err != nil || !matches {
		t.Fatalf("the queue row is not its event's: %v, %v", matches, err)
	}
}

// Review 4.1 N1: a replayed key that belongs to a plain message is no enqueue.
func TestEnqueueRefusesAPlainMessagesKey(t *testing.T) {
	ctx := t.Context()
	s, _, _, _ := open(t)
	plain := queuedDraft(1, "said")
	plain.Payload = envelope.Must(envelope.MessagePayload{Kind: envelope.KindChat, Text: "said", Delivery: envelope.DeliveryNone})
	if _, _, err := s.Append(ctx, plain); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enqueue(ctx, queuedDraft(1, "said"), "human:alice", "said"); !errors.Is(err, ErrKeyConflict) {
		t.Fatalf("a plain message's key replayed as an enqueue: %v", err)
	}
	if got, err := s.Queue(ctx, room); err != nil || len(got) != 0 {
		t.Fatalf("%+v, %v", got, err)
	}
}

// Review 4.1 N2: a sealed room's queue does not move.
func TestSetQueuedInASealedRoom(t *testing.T) {
	ctx := t.Context()
	s, _, _, _ := open(t)
	ev, err := s.Enqueue(ctx, queuedDraft(1, "late"), "human:alice", "late")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CloseRoom(ctx, room, "done"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetQueued(ctx, room, ev.Seq, "consumed", "7f3cq2xz"); !errors.Is(err, ErrSealed) {
		t.Fatalf("%v", err)
	}
	if err := s.SetQueued(ctx, room, ev.Seq+100, "consumed", "7f3cq2xz"); !errors.Is(err, ErrSealed) {
		t.Fatalf("a sealed room answers sealed first: %v", err)
	}
}

// SP3 R9: a system caller's queued message, as the broker's queue route builds it,
// passes queue_is_its_event, keeps its stream's cursor and is consumed once.
func TestASystemCallerQueuesAndConsumes(t *testing.T) {
	ctx := t.Context()
	s, _, _, _ := open(t)
	d := queuedDraft(901, "use the relative link")
	d.Actor = envelope.Actor{Kind: envelope.ActorSystem, ID: "system:factory"}
	d.OriginClient = "system:factory:queue:review"
	ev, err := s.Enqueue(ctx, d, "system:factory", "use the relative link")
	if err != nil {
		t.Fatal(err)
	}
	if cur, err := s.Cursor(ctx, room, d.OriginClient); err != nil || cur != 901 {
		t.Fatalf("cursor %d, %v", cur, err)
	}
	if got, err := s.Queue(ctx, room); err != nil || len(got) != 1 || got[0].Ref != ev.Seq || got[0].Author != "system:factory" {
		t.Fatalf("%+v, %v", got, err)
	}
	if err := s.SetQueued(ctx, room, ev.Seq, "consumed", "aaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetQueued(ctx, room, ev.Seq, "consumed", "aaaaaaaa"); !errors.Is(err, ErrNotQueued) {
		t.Fatalf("consumed twice: %v", err)
	}
	if got, err := s.Queue(ctx, room); err != nil || len(got) != 0 {
		t.Fatalf("%+v, %v", got, err)
	}
}

// §2: a driver-only append applies only while its sender holds the token at the
// epoch it decided on, checked under the row lock.
func TestAppendAsDriverIsFenced(t *testing.T) {
	ctx := t.Context()
	s, _, _, _ := open(t)
	if _, _, err := s.ChangeDriver(ctx, room, 0, "human:alice", "requested", humanDraft("human:alice", 1)); err != nil {
		t.Fatal(err)
	}
	steer := func(n int64) envelope.Draft {
		d := queuedDraft(n, "now")
		d.Payload = envelope.Must(envelope.MessagePayload{Kind: envelope.KindChat, Text: "now", Delivery: envelope.DeliverySteering})
		return d
	}
	for _, c := range []struct {
		name, driver string
		epoch        int64
	}{{"a stale epoch", "human:alice", 0}, {"another principal", "human:bob", 1}, {"no driver", "", 1}} {
		if _, _, err := s.AppendAsDriver(ctx, c.driver, c.epoch, steer(2)); !errors.Is(err, ErrStaleEpoch) {
			t.Fatalf("%s: %v", c.name, err)
		}
	}
	if ev, _, err := s.AppendAsDriver(ctx, "human:alice", 1, steer(2)); err != nil || ev.Seq != 2 {
		t.Fatalf("the holder at its epoch: %+v, %v", ev, err)
	}
}

func driverDraft(n int64) envelope.Draft {
	return envelope.Draft{RoomID: room, Actor: envelope.Actor{Kind: envelope.ActorHuman, ID: "human:own"},
		Origin: envelope.OriginClient, OriginClient: "human:own:s1", OriginSeq: n}
}

// Promotion is fenced and atomic: the row and the steering message move together.
func TestPromoteQueued(t *testing.T) {
	ctx := t.Context()
	s, _, _, _ := open(t)
	q, err := s.Enqueue(ctx, queuedDraft(1, "address L42"), "human:alice", "address L42")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ChangeDriver(ctx, room, 0, "human:own", "given", driverDraft(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PromoteQueued(ctx, room, q.Seq, "human:own", 0, "7f3cq2xz", driverDraft(2)); !errors.Is(err, ErrStaleEpoch) {
		t.Fatalf("a stale epoch: %v", err)
	}
	if got, _ := s.Queue(ctx, room); len(got) != 1 {
		t.Fatalf("a refused promotion moved the row: %+v", got)
	}
	ev, err := s.PromoteQueued(ctx, room, q.Seq, "human:own", 1, "7f3cq2xz", driverDraft(2))
	if err != nil {
		t.Fatal(err)
	}
	var p envelope.MessagePayload
	_ = json.Unmarshal(ev.Payload, &p)
	if ev.Type != envelope.Message || ev.CausedBy == nil || *ev.CausedBy != q.Seq || p.Text != "address L42" ||
		p.Delivery != envelope.DeliverySteering || len(p.To) != 1 || p.To[0] != "agent:7f3cq2xz" {
		t.Fatalf("steering event %+v %+v", ev, p)
	}
	var state, run string
	if err := s.pool.QueryRow(ctx, `SELECT state, run_id FROM queue WHERE room_id = $1 AND ref = $2`, room, q.Seq).Scan(&state, &run); err != nil ||
		state != "promoted" || run != "7f3cq2xz" {
		t.Fatalf("row %s %s %v", state, run, err)
	}
	if again, err := s.PromoteQueued(ctx, room, q.Seq, "human:own", 1, "7f3cq2xz", driverDraft(2)); err != nil || again.Seq != ev.Seq {
		t.Fatalf("a replay returns the stored event: %+v, %v", again, err)
	}
	if _, err := s.PromoteQueued(ctx, room, q.Seq, "human:own", 1, "7f3cq2xz", driverDraft(3)); !errors.Is(err, ErrNotQueued) {
		t.Fatalf("promoted twice: %v", err)
	}
	if _, err := s.PromoteQueued(ctx, room, q.Seq, "human:own", 1, "7f3cq2xz", queuedDraft(1, "address L42")); !errors.Is(err, ErrKeyConflict) {
		t.Fatalf("a queued message's key replayed as a promotion: %v", err)
	}
}

// §2: the author or the driver withdraws a queued message, recorded in the same
// transaction.
func TestRemoveQueued(t *testing.T) {
	ctx := t.Context()
	s, _, _, _ := open(t)
	a, _ := s.Enqueue(ctx, queuedDraft(1, "one"), "human:alice", "one")
	b, _ := s.Enqueue(ctx, queuedDraft(2, "two"), "human:alice", "two")
	if _, _, err := s.ChangeDriver(ctx, room, 0, "human:own", "given", driverDraft(1)); err != nil {
		t.Fatal(err)
	}
	bob := envelope.Draft{RoomID: room, Actor: envelope.Actor{Kind: envelope.ActorHuman, ID: "human:bob"},
		Origin: envelope.OriginClient, OriginClient: "human:bob:s1", OriginSeq: 1}
	if _, err := s.RemoveQueued(ctx, room, a.Seq, bob); !errors.Is(err, ErrNotAuthor) {
		t.Fatalf("neither author nor driver: %v", err)
	}
	byAuthor := queuedDraft(3, "")
	ev, err := s.RemoveQueued(ctx, room, a.Seq, byAuthor)
	if err != nil || ev.Type != envelope.StateChanged || string(ev.Payload) != `{"kind":"queued_removed","ref":`+itoa(a.Seq)+`}` {
		t.Fatalf("the author removes: %+v %s, %v", ev, ev.Payload, err)
	}
	if again, err := s.RemoveQueued(ctx, room, a.Seq, byAuthor); err != nil || again.Seq != ev.Seq {
		t.Fatalf("a replay returns the stored event: %+v, %v", again, err)
	}
	if _, err := s.RemoveQueued(ctx, room, b.Seq, byAuthor); !errors.Is(err, ErrKeyConflict) {
		t.Fatalf("one removal's key replayed for another message: %v", err)
	}
	if _, err := s.RemoveQueued(ctx, room, b.Seq, queuedDraft(1, "")); !errors.Is(err, ErrKeyConflict) {
		t.Fatalf("a queued message's key replayed as a removal: %v", err)
	}
	if _, err := s.RemoveQueued(ctx, room, b.Seq, driverDraft(2)); err != nil {
		t.Fatalf("the driver removes: %v", err)
	}
	if _, err := s.RemoveQueued(ctx, room, b.Seq, driverDraft(3)); !errors.Is(err, ErrNotQueued) {
		t.Fatalf("removed twice: %v", err)
	}
	if got, _ := s.Queue(ctx, room); len(got) != 0 {
		t.Fatalf("%+v", got)
	}
}

func itoa(n int64) string { return string(envelope.Must(n)) }

// Review 4.2 M1: the enqueue that reaches the room's limit seals it, and the seal
// stays: the human is told sealed, now and on every retry.
func TestEnqueueAtTheLimit(t *testing.T) {
	ctx := t.Context()
	s, _, _, _ := open(t)
	s.MaxEvents = 3
	if _, _, err := s.Append(ctx, draft("agent:x", 1)); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := s.Enqueue(ctx, queuedDraft(1, "too late"), "human:alice", "too late"); !errors.Is(err, ErrSealed) {
			t.Fatalf("at the limit: %v", err)
		}
	}
	if st, err := s.Room(ctx, room); err != nil || !st.Sealed || st.LastSeq != 3 {
		t.Fatalf("the seal did not survive: %+v, %v", st, err)
	}
	if got, err := s.Queue(ctx, room); err != nil || len(got) != 0 {
		t.Fatalf("%+v, %v", got, err)
	}
}

// Review 4.2 M4: a key replayed for another event type is a conflict, not the
// first event reported as this one.
func TestAppendRefusesAnotherTypesKey(t *testing.T) {
	ctx := t.Context()
	s, _, _, _ := open(t)
	chat := queuedDraft(1, "hi")
	chat.Payload = envelope.Must(envelope.MessagePayload{Kind: envelope.KindChat, Text: "hi", Delivery: envelope.DeliveryNone})
	first, _, err := s.Append(ctx, chat)
	if err != nil {
		t.Fatal(err)
	}
	if again, dup, err := s.Append(ctx, chat); err != nil || !dup || again.Seq != first.Seq {
		t.Fatalf("a same-type replay: %+v %v %v", again, dup, err)
	}
	other := chat
	other.Type, other.Payload = envelope.StateChanged, envelope.StatePayload("interrupt", map[string]any{"runId": "7f3cq2xz"})
	if _, _, err := s.Append(ctx, other); !errors.Is(err, ErrKeyConflict) {
		t.Fatalf("Append: %v", err)
	}
	if _, _, err := s.ChangeDriver(ctx, room, 0, "human:alice", "requested", humanDraft("human:alice", 2)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AppendAsDriver(ctx, "human:alice", 1, other); !errors.Is(err, ErrKeyConflict) {
		t.Fatalf("AppendAsDriver: %v", err)
	}
}

// Task 4.3: a queued message moves once, out of queued; promoted and consumed
// name their run, removed names none.
func TestQueueStateMachine(t *testing.T) {
	ctx := t.Context()
	s, _, _, _ := open(t)
	ev, err := s.Enqueue(ctx, queuedDraft(1, "once"), "human:alice", "once")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ to, run string }{
		{"queued", ""}, {"delivered", "7f3cq2xz"}, // no such edge
		{"consumed", ""}, {"promoted", ""}, // the run unnamed
		{"removed", "7f3cq2xz"}, {"consumed", "NOT A RUN"},
	} {
		if err := s.SetQueued(ctx, room, ev.Seq, c.to, c.run); !errors.Is(err, ErrBadMove) {
			t.Fatalf("-> %s (%q): %v", c.to, c.run, err)
		}
	}
	if err := s.SetQueued(ctx, room, ev.Seq, "consumed", "7f3cq2xz"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetQueued(ctx, room, ev.Seq, "removed", ""); !errors.Is(err, ErrNotQueued) {
		t.Fatalf("a consumed message moves again: %v", err)
	}
}
