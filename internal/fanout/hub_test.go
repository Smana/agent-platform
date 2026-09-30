// SPDX-License-Identifier: Apache-2.0

package fanout

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/store"
)

const roomID = "3kq7x2ma"

var discard = slog.New(slog.DiscardHandler)

// memLog is the store's read side. gate, when set, holds every Range until it
// closes; while fails is above zero, each Range fails and counts it down.
type memLog struct {
	mu     sync.Mutex
	evs    []envelope.Event
	ranges atomic.Int32
	gate   chan struct{}
	fails  atomic.Int32
}

func (m *memLog) add(n int) int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	for range n {
		m.evs = append(m.evs, envelope.Event{Seq: int64(len(m.evs) + 1), RoomID: roomID, Payload: []byte(`{"k":1}`), TS: time.Now()})
	}
	return int64(len(m.evs))
}

func (m *memLog) Range(ctx context.Context, _ string, after int64, limit int) ([]envelope.Event, error) {
	m.ranges.Add(1)
	if n := m.fails.Load(); n > 0 && m.fails.CompareAndSwap(n, n-1) {
		return nil, errors.New("connection reset by peer")
	}
	if m.gate != nil {
		select {
		case <-m.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []envelope.Event
	for _, e := range m.evs {
		if e.Seq > after && len(out) < limit {
			out = append(out, e)
		}
	}
	return out, nil
}

func (m *memLog) Room(_ context.Context, id string) (store.RoomState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return store.RoomState{ID: id, LastSeq: int64(len(m.evs))}, nil
}

// bus stands in for PostgreSQL's NOTIFY: every live Listen session, on every
// replica, hears every publish.
type bus struct {
	mu       sync.Mutex
	sessions map[*session]struct{}
	opened   chan struct{} // one tick per Listen that reached ready
}

type session struct {
	notify func(string, int64)
	kill   chan error
}

func newBus() *bus { return &bus{sessions: map[*session]struct{}{}, opened: make(chan struct{}, 16)} }

func (b *bus) Listen(ctx context.Context, ready func(), notify func(string, int64)) error {
	s := &session{notify: notify, kill: make(chan error, 1)}
	b.mu.Lock()
	b.sessions[s] = struct{}{}
	b.mu.Unlock()
	defer func() {
		b.mu.Lock()
		delete(b.sessions, s)
		b.mu.Unlock()
	}()
	ready()
	b.opened <- struct{}{}
	select {
	case <-ctx.Done():
		return nil
	case err := <-s.kill:
		return err
	}
}

func (b *bus) publish(room string, seq int64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for s := range b.sessions {
		s.notify(room, seq)
	}
}

// killAll drops every session, as pg_terminate_backend does.
func (b *bus) killAll() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for s := range b.sessions {
		s.kill <- errors.New("terminating connection due to administrator command")
	}
}

// run starts h until the test ends; every loop test is bounded at 5 s.
func run(t *testing.T, h *Hub) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("run: %v", err)
		}
	})
	return ctx
}

func waitOpened(t *testing.T, b *bus) {
	t.Helper()
	select {
	case <-b.opened:
	case <-time.After(5 * time.Second):
		t.Fatal("the hub never listened")
	}
}

// drain reads n events, acknowledging each, and fails on a gap or a duplicate.
func drain(t *testing.T, s *Sub, from int64, n int) {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for want := from; want < from+int64(n); want++ {
		select {
		case e := <-s.C:
			s.Sent(e)
			if e.Seq != want {
				t.Fatalf("got seq %d, want %d: a gap or a duplicate", e.Seq, want)
			}
		case <-deadline:
			t.Fatalf("stuck before seq %d", want)
		}
	}
}

func expectQuiet(t *testing.T, s *Sub) {
	t.Helper()
	select {
	case e := <-s.C:
		t.Fatalf("delivered seq %d, want nothing yet", e.Seq)
	case <-time.After(200 * time.Millisecond):
	}
}

// Any replica appends; every replica's viewers hear it, and a new subscriber
// gets only what is new.
func TestNotifyReachesEveryReplica(t *testing.T) {
	log, b := &memLog{}, newBus()
	log.add(3)
	a, c := New(log, b, discard), New(log, b, discard)
	ctxA, ctxC := run(t, a), run(t, c)
	waitOpened(t, b)
	waitOpened(t, b)
	subA, err := a.Subscribe(ctxA, roomID)
	if err != nil {
		t.Fatal(err)
	}
	subC, err := c.Subscribe(ctxC, roomID)
	if err != nil {
		t.Fatal(err)
	}
	b.publish(roomID, log.add(2))
	drain(t, subA, 4, 2)
	drain(t, subC, 4, 2)
}

// Ruling AT: notifications coalesce per room. 100 appends while a read is in
// flight cost at most a few reads, and the subscriber still sees every seq once.
func TestNotificationsCoalesce(t *testing.T) {
	log, b := &memLog{gate: make(chan struct{})}, newBus()
	h := New(log, b, discard)
	ctx := run(t, h)
	waitOpened(t, b)
	sub, err := h.Subscribe(ctx, roomID) // its catch-up read is the first held Range
	if err != nil {
		t.Fatal(err)
	}
	for range 100 {
		b.publish(roomID, log.add(1))
	}
	close(log.gate)
	drain(t, sub, 1, 100)
	if n := log.ranges.Load(); n > 4 {
		t.Fatalf("%d reads for 100 appends, want a few", n)
	}
}

// With no listener at all, the hub polls every subscribed room.
func TestPollsWithoutAListener(t *testing.T) {
	log := &memLog{}
	h := New(log, nil, discard)
	h.PollEvery = 20 * time.Millisecond
	ctx := run(t, h)
	sub, err := h.Subscribe(ctx, roomID)
	if err != nil {
		t.Fatal(err)
	}
	log.add(3) // appended elsewhere, never notified
	drain(t, sub, 1, 3)
}

// While the listener is down the hub polls; the reconnect catches every
// subscribed room up from its last delivered seq, with no gap and no duplicate.
func TestListenerLoss(t *testing.T) {
	t.Run("polling takes over", func(t *testing.T) {
		log, b := &memLog{}, newBus()
		h := New(log, b, discard)
		h.PollEvery = 20 * time.Millisecond
		h.After = func(d time.Duration) <-chan time.Time {
			if d == h.PollEvery {
				return time.After(d)
			}
			return nil // the reconnect never comes
		}
		ctx := run(t, h)
		waitOpened(t, b)
		sub, err := h.Subscribe(ctx, roomID)
		if err != nil {
			t.Fatal(err)
		}
		b.publish(roomID, log.add(2))
		drain(t, sub, 1, 2)
		b.killAll()
		waitUnhealthy(t, h)
		log.add(3)
		drain(t, sub, 3, 3)
	})
	t.Run("the reconnect catches up", func(t *testing.T) {
		log, b := &memLog{}, newBus()
		h := New(log, b, discard)
		h.PollEvery = time.Hour // isolate the catch-up from the poll
		backoff := make(chan time.Time)
		h.After = func(d time.Duration) <-chan time.Time {
			if d == h.PollEvery {
				return nil
			}
			return backoff
		}
		ctx := run(t, h)
		waitOpened(t, b)
		sub, err := h.Subscribe(ctx, roomID)
		if err != nil {
			t.Fatal(err)
		}
		b.publish(roomID, log.add(2))
		drain(t, sub, 1, 2)
		b.killAll()
		waitUnhealthy(t, h)
		log.add(3) // notified to nobody
		expectQuiet(t, sub)
		backoff <- time.Now()
		waitOpened(t, b)
		drain(t, sub, 3, 3)
		b.publish(roomID, log.add(1))
		drain(t, sub, 6, 1)
		expectQuiet(t, sub)
	})
}

func waitUnhealthy(t *testing.T, h *Hub) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for h.Healthy() {
		if time.Now().After(deadline) {
			t.Fatal("still healthy after the listener died")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Reconnects back off 1 s, doubling, capped at 30 s, plus up to a quarter of
// jitter, on the injected clock; a session that reached LISTEN resets it.
func TestReconnectBackoff(t *testing.T) {
	var calls atomic.Int32
	failing := listenFunc(func(_ context.Context, ready func(), _ func(string, int64)) error {
		if calls.Add(1) == 7 {
			ready() // the seventh session reaches LISTEN, then drops
		}
		return errors.New("connection refused")
	})
	h := New(&memLog{}, failing, discard)
	h.PollEvery = time.Hour
	waits := make(chan time.Duration, 16)
	h.After = func(d time.Duration) <-chan time.Time {
		if d == h.PollEvery {
			return nil
		}
		waits <- d
		c := make(chan time.Time, 1)
		c <- time.Now()
		return c
	}
	run(t, h)
	for i, base := range []time.Duration{1, 2, 4, 8, 16, 30, 1} {
		base *= time.Second
		select {
		case d := <-waits:
			if d < base || d >= base+base/4 {
				t.Fatalf("wait %d: %s, want %s plus under a quarter", i, d, base)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("wait %d never came", i)
		}
	}
}

type listenFunc func(ctx context.Context, ready func(), notify func(string, int64)) error

func (f listenFunc) Listen(ctx context.Context, ready func(), notify func(string, int64)) error {
	return f(ctx, ready, notify)
}

// §4: a connection over its pending budget is dropped; it resumes from afterSeq.
func TestSlowConsumerIsDropped(t *testing.T) {
	log, b := &memLog{}, newBus()
	h := New(log, b, discard)
	h.Budget = 20 // two tiny events
	ctx := run(t, h)
	waitOpened(t, b)
	slow, err := h.Subscribe(ctx, roomID)
	if err != nil {
		t.Fatal(err)
	}
	fast, err := h.Subscribe(ctx, roomID)
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		b.publish(roomID, log.add(1))
		drain(t, fast, log.add(0), 1) // the other viewer keeps up, unaffected
	}
	select {
	case <-slow.Dropped:
	case <-time.After(5 * time.Second):
		t.Fatal("not dropped")
	}
}

// A viewer that stops reading fills its channel long before a big budget: it is
// dropped there, and the worker, which holds the hub's lock while it offers,
// never waits on it. Every other viewer of the room keeps every seq (review I1).
func TestAStalledViewerIsDroppedAtItsChannelCap(t *testing.T) {
	log, b := &memLog{}, newBus()
	h := New(log, b, discard)
	h.Budget = 1 << 30
	ctx := run(t, h)
	waitOpened(t, b)
	stalled, err := h.Subscribe(ctx, roomID)
	if err != nil {
		t.Fatal(err)
	}
	live, err := h.Subscribe(ctx, roomID)
	if err != nil {
		t.Fatal(err)
	}
	// Runs before run's cleanup: were the offer to block, the worker would hang on
	// stalled for good and the test would die on go test's timeout, not its own.
	t.Cleanup(func() {
		for {
			select {
			case <-stalled.C:
			case <-time.After(200 * time.Millisecond):
				return
			}
		}
	})
	const batch = 1000
	for i := range 5 { // 5000 events: past the 4096-slot channel
		b.publish(roomID, log.add(batch))
		drain(t, live, int64(i*batch+1), batch)
	}
	select {
	case <-stalled.Dropped:
	case <-time.After(5 * time.Second):
		t.Fatal("the stalled viewer was never dropped")
	}
}

// A read that started before its room was forgotten and subscribed again must
// not offer the new viewer what it already holds from the log (review M7, F6).
func TestAReadForAForgottenRoomIsDiscarded(t *testing.T) {
	log, b := &memLog{gate: make(chan struct{})}, newBus()
	h := New(log, b, discard)
	ctx := run(t, h)
	waitOpened(t, b)
	first, err := h.Subscribe(ctx, roomID) // its read (after 0) is held on the gate
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for log.ranges.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the first read never started")
		}
		time.Sleep(5 * time.Millisecond)
	}
	log.add(5)
	h.Unsubscribe(first)
	second, err := h.Subscribe(ctx, roomID) // starts after seq 5
	if err != nil {
		t.Fatal(err)
	}
	close(log.gate) // the stale read now returns 1..5
	expectQuiet(t, second)
	b.publish(roomID, log.add(1))
	drain(t, second, 6, 1)
}

// A failed read is logged and retried by the next poll with the listener still
// up: the notification that prompted it is not repeated.
func TestFailedReadIsRetried(t *testing.T) {
	log, b := &memLog{}, newBus()
	var logs syncBuffer
	h := New(log, b, slog.New(slog.NewTextHandler(&logs, nil)))
	h.PollEvery = 20 * time.Millisecond
	ctx := run(t, h)
	waitOpened(t, b)
	sub, err := h.Subscribe(ctx, roomID)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // the subscribe's own read has run
	log.fails.Store(1)
	b.publish(roomID, log.add(2))
	drain(t, sub, 1, 2)
	if !h.Healthy() {
		t.Fatal("the retry must not need the listener down")
	}
	if !bytes.Contains(logs.Bytes(), []byte("fan-out read failed")) {
		t.Fatalf("the failed read was not logged: %q", logs.Bytes())
	}
}

// syncBuffer is a log sink the hub's goroutines and the test share.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) Bytes() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return bytes.Clone(s.b.Bytes())
}

// The last viewer leaving forgets the room: its notifications cost nothing.
func TestUnsubscribeForgetsTheRoom(t *testing.T) {
	log, b := &memLog{}, newBus()
	h := New(log, b, discard)
	ctx := run(t, h)
	waitOpened(t, b)
	sub, err := h.Subscribe(ctx, roomID)
	if err != nil {
		t.Fatal(err)
	}
	b.publish(roomID, log.add(1))
	drain(t, sub, 1, 1)
	h.Unsubscribe(sub)
	before := log.ranges.Load()
	b.publish(roomID, log.add(1))
	time.Sleep(100 * time.Millisecond)
	if n := log.ranges.Load(); n != before {
		t.Fatalf("%d reads for a room nobody watches", n-before)
	}
}
