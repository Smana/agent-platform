// SPDX-License-Identifier: Apache-2.0

// Package fanout delivers a room's new events to this replica's viewers (§4,
// Ruling AT). Any replica serves any room: every append NOTIFYs inside its own
// transaction, each replica LISTENs, and its hub reads a notified room's new
// events from the log once, for every local viewer. The listener only marks rooms
// dirty and wakes one worker; it never reads the log, so a slow read or a slow
// viewer cannot back up PostgreSQL's notification queue and stall appends.
// While the listener is down the hub polls every subscribed room each PollEvery;
// a reconnect catches every room up from the last seq it delivered.
package fanout

import (
	"context"
	"crypto/rand"
	"log/slog"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/store"
)

// pageSize bounds one log read.
const pageSize = 500

// Reader is the log's read side.
type Reader interface {
	Range(ctx context.Context, roomID string, afterSeq int64, limit int) ([]envelope.Event, error)
	Room(ctx context.Context, id string) (store.RoomState, error)
}

// Listener holds one LISTEN session: ready once it listens, notify per
// notification, nil when ctx ends, an error when the session is lost.
type Listener interface {
	Listen(ctx context.Context, ready func(), notify func(roomID string, seq int64)) error
}

// Sub is one viewer's feed. C carries the room's new events in seq order;
// Dropped closes when the viewer fell behind by more than the budget or the
// channel's capacity, and it must resume from its afterSeq (§4).
type Sub struct {
	C       <-chan envelope.Event
	Dropped <-chan struct{}

	room    string
	c       chan envelope.Event
	dropped chan struct{}
	budget  int

	mu      sync.Mutex
	pending int
	closed  bool
}

// Sent tells the hub the connection wrote ev, freeing its share of the budget.
func (s *Sub) Sent(ev envelope.Event) {
	s.mu.Lock()
	s.pending -= len(ev.Payload)
	s.mu.Unlock()
}

// offer never blocks: a viewer that cannot take ev is dropped instead.
func (s *Sub) offer(ev envelope.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return
	}
	if s.pending+len(ev.Payload) > s.budget {
		s.drop()
		return
	}
	select {
	case s.c <- ev:
		s.pending += len(ev.Payload)
	default:
		s.drop()
	}
}

func (s *Sub) drop() {
	s.closed = true
	close(s.dropped)
}

// room is one subscribed room's state, guarded by Hub.mu.
type room struct {
	subs  map[*Sub]struct{}
	last  int64 // the last seq offered to subs: the resume cursor
	dirty bool  // the log may hold events after last
	retry bool  // the last read failed: the next poll retries it, listener up or not
}

// Hub fans one replica's rooms out to its viewers. Set the exported fields
// before Run.
type Hub struct {
	// PollEvery is the poll period while the listener is down (default 1 s).
	PollEvery time.Duration
	// Budget is a viewer's pending bytes before it is dropped (default 2 MiB).
	Budget int
	// MinBackoff and MaxBackoff bound the reconnect wait, which doubles per
	// failed session and gains up to a quarter of jitter (default 1 s and 30 s).
	MinBackoff, MaxBackoff time.Duration
	// After is the clock (default time.After).
	After func(time.Duration) <-chan time.Time

	log     *slog.Logger
	r       Reader
	l       Listener
	healthy atomic.Bool
	wake    chan struct{} // capacity 1: a wake-up already pending covers any more

	mu    sync.Mutex
	rooms map[string]*room
}

// New builds a hub over the log and a listener; a nil listener polls only. log
// reports lost listener sessions and failed reads.
func New(r Reader, l Listener, log *slog.Logger) *Hub {
	return &Hub{PollEvery: time.Second, Budget: 2 << 20, MinBackoff: time.Second, MaxBackoff: 30 * time.Second,
		After: time.After, log: log, r: r, l: l, wake: make(chan struct{}, 1), rooms: map[string]*room{}}
}

// Healthy reports whether the listener is in place, so notifications flow.
func (h *Hub) Healthy() bool { return h.healthy.Load() }

// Subscribe starts buffering the room's events after its current last seq. The
// caller then reads the log up to its mark and drops buffered events at or below
// it (§4 Replay, after OpenHands #4681).
func (h *Hub) Subscribe(ctx context.Context, roomID string) (*Sub, error) {
	h.mu.Lock()
	_, known := h.rooms[roomID]
	h.mu.Unlock()
	var last int64
	if !known {
		st, err := h.r.Room(ctx, roomID)
		if err != nil {
			return nil, err
		}
		last = st.LastSeq
	}
	c, d := make(chan envelope.Event, 4096), make(chan struct{})
	s := &Sub{C: c, Dropped: d, room: roomID, c: c, dropped: d, budget: h.Budget}
	h.mu.Lock()
	rm, ok := h.rooms[roomID]
	if !ok {
		// Notifications for a room nobody watched were ignored: read once, in case
		// an append landed between the Room read and now.
		rm = &room{subs: map[*Sub]struct{}{}, last: last, dirty: true}
		h.rooms[roomID] = rm
	}
	rm.subs[s] = struct{}{}
	h.mu.Unlock()
	h.kick()
	return s, nil
}

// Unsubscribe stops s. The room's last viewer leaving forgets the room.
func (h *Hub) Unsubscribe(s *Sub) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if rm, ok := h.rooms[s.room]; ok {
		delete(rm.subs, s)
		if len(rm.subs) == 0 {
			delete(h.rooms, s.room)
		}
	}
}

// Run listens, reconnecting with backoff, polls while the listener is down, and
// runs the one worker that reads the log. It returns nil when ctx ends.
func (h *Hub) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	wg.Go(func() { h.work(ctx) })
	wg.Go(func() { h.poll(ctx) })
	if h.l != nil {
		h.listen(ctx)
	}
	<-ctx.Done()
	wg.Wait()
	return nil
}

// listen keeps one Listener session up. Each session starts with a catch-up of
// every subscribed room, since notifications sent while none listened are lost.
func (h *Hub) listen(ctx context.Context) {
	backoff := h.MinBackoff
	for {
		var reached atomic.Bool
		err := h.l.Listen(ctx, func() {
			reached.Store(true)
			h.healthy.Store(true)
			h.markAll(false)
		}, h.notify)
		h.healthy.Store(false)
		if ctx.Err() != nil {
			return
		}
		if reached.Load() {
			backoff = h.MinBackoff
		}
		h.log.Warn("fan-out listener lost; polling until it reconnects", "error", err, "retryIn", backoff)
		select {
		case <-ctx.Done():
			return
		case <-h.After(backoff + jitter(backoff/4)):
		}
		backoff = min(2*backoff, h.MaxBackoff)
	}
}

// notify runs on the listener's goroutine: it only marks the room and wakes the
// worker, never reads the log.
func (h *Hub) notify(roomID string, seq int64) {
	h.mu.Lock()
	rm, ok := h.rooms[roomID]
	if ok && seq > rm.last {
		rm.dirty = true
	}
	h.mu.Unlock()
	if ok {
		h.kick()
	}
}

// kick wakes the worker without ever blocking: a full channel means a wake-up
// is already pending, and the dirty flags carry what it must do.
func (h *Hub) kick() {
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

// markAll marks every subscribed room dirty, or only those whose last read
// failed, and wakes the worker.
func (h *Hub) markAll(retriesOnly bool) {
	h.mu.Lock()
	for _, rm := range h.rooms {
		if !retriesOnly || rm.retry {
			rm.dirty = true
		}
	}
	h.mu.Unlock()
	h.kick()
}

// poll reads every subscribed room each PollEvery while the listener is down;
// while it is up, only rooms whose last read failed.
func (h *Hub) poll(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.After(h.PollEvery):
		}
		h.markAll(h.Healthy())
	}
}

// work is the hub's only reader of the log: one goroutine, whatever the load.
func (h *Hub) work(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-h.wake:
		}
		for _, id := range h.takeDirty() {
			h.fetch(ctx, id)
		}
	}
}

func (h *Hub) takeDirty() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var ids []string
	for id, rm := range h.rooms {
		if rm.dirty {
			rm.dirty = false
			ids = append(ids, id)
		}
	}
	return ids
}

// fetch offers the room's events after its cursor to every viewer, page by page.
// No lock is held across the read.
func (h *Hub) fetch(ctx context.Context, id string) {
	for {
		h.mu.Lock()
		rm, ok := h.rooms[id]
		var after int64
		if ok {
			after = rm.last
		}
		h.mu.Unlock()
		if !ok {
			return
		}
		evs, err := h.r.Range(ctx, id, after, pageSize)
		if err != nil && ctx.Err() == nil {
			h.log.Warn("fan-out read failed; the next poll retries", "room", id, "afterSeq", after, "error", err)
		}
		h.mu.Lock()
		rm, ok = h.rooms[id]
		if ok && err != nil {
			rm.retry = true
		}
		if !ok || err != nil || rm.last != after {
			// Forgotten, failed (the poll retries it) or resubscribed meanwhile.
			h.mu.Unlock()
			return
		}
		rm.retry = false
		for _, ev := range evs {
			for s := range rm.subs {
				s.offer(ev) // never blocks
			}
			rm.last = ev.Seq
		}
		h.mu.Unlock()
		if len(evs) < pageSize {
			return
		}
	}
}

// jitter is a uniform duration in [0, upTo).
func jitter(upTo time.Duration) time.Duration {
	if upTo <= 0 {
		return 0
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(upTo)))
	if err != nil {
		return 0
	}
	return time.Duration(n.Int64())
}
