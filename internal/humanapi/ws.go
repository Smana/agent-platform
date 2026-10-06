// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/policy"
	"github.com/Smana/agent-platform/internal/store"
	"github.com/Smana/agent-platform/internal/wire"
)

const (
	maxPerUser  = 10 // §4
	maxPerRoom  = 20 // humans per room (§4), per replica (ruling P22)
	defaultTail = 500
	pageSize    = 500
	// maxClientFrame bounds one client frame: an act carries at most a human
	// message (§4) and its envelope. Larger closes the socket 1009. It equals
	// coder/websocket's default read limit today: set it anyway, so a larger
	// MaxHumanMessage is not silently capped at 32 KiB.
	maxClientFrame = 2 * envelope.MaxHumanMessage

	defaultMaxLifetime = time.Hour

	defaultHelloWait = 10 * time.Second
	defaultWriteWait = 10 * time.Second
	defaultPingEvery = 30 * time.Second
	defaultPongWait  = 10 * time.Second

	// closeReauth asks the client to reconnect with a fresh token (docs/api.md).
	closeReauth websocket.StatusCode = 4001
)

// Why a connection ended, as its life's cause or a failed operation's error.
var (
	errSlowConsumer = errors.New(dropSlowConsumer)
	errWriteTimeout = errors.New(dropWriteTimeout)
	errPingTimeout  = errors.New(dropPingTimeout)
	errProtocol     = errors.New(dropProtocol)
	errClientGone   = errors.New(dropClientGone)
	errShutdown     = errors.New(dropShutdown)
	errLog          = errors.New(dropLogUnavailable)
)

func or(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}

// acquire takes a connection slot for user in room, or refuses: 10 per person,
// and 20 people per room, where a person already in the room may open another tab.
func (s *Server) acquire(user, room string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.perUser == nil {
		s.perUser, s.perRoom = map[string]int{}, map[string]map[string]int{}
	}
	in := s.perRoom[room]
	if s.perUser[user] >= maxPerUser || (in[user] == 0 && len(in) >= maxPerRoom) {
		return false
	}
	if in == nil {
		in = map[string]int{}
		s.perRoom[room] = in
	}
	s.perUser[user]++
	in[user]++
	return true
}

func (s *Server) release(user, room string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.perUser[user]--; s.perUser[user] <= 0 {
		delete(s.perUser, user)
	}
	if s.perRoom[room][user]--; s.perRoom[room][user] <= 0 {
		delete(s.perRoom[room], user)
	}
	if len(s.perRoom[room]) == 0 {
		delete(s.perRoom, room)
	}
}

// track registers a connection so shutdown can end it; untrack when it ends.
func (s *Server) track(cancel *context.CancelCauseFunc) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return false
	}
	if s.conns == nil {
		s.conns = map[*context.CancelCauseFunc]struct{}{}
	}
	s.conns[cancel] = struct{}{}
	return true
}

func (s *Server) untrack(cancel *context.CancelCauseFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.conns, cancel)
}

func (s *Server) open() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.conns)
}

// closeAll ends every connection and refuses new ones: the server is shutting down.
func (s *Server) closeAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closing = true
	for c := range s.conns {
		(*c)(errShutdown)
	}
}

// ws is GET /v1/ws?room=: every refusal is a plain HTTP status before the upgrade.
func (s *Server) ws(w http.ResponseWriter, r *http.Request) {
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	id := r.URL.Query().Get("room")
	room, st, ok := s.lookup(w, r, id)
	if !ok {
		return
	}
	// The Read gate only: serve resolves the snapshot's standing again against
	// the mark it reads after subscribing, so the two agree.
	sub, _ := s.you(room, p, st.Driver)
	if !policy.Allowed(sub, policy.Read) {
		http.Error(w, wire.ReasonNotPermitted, http.StatusForbidden)
		return
	}
	if !s.acquire(p.ID, id) {
		http.Error(w, "too many connections", http.StatusTooManyRequests)
		return
	}
	defer s.release(p.ID, id)
	// Authenticate checked Origin against the configured origin; Accept checks it
	// against Host as well (T9).
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer func() { _ = c.CloseNow() }()
	c.SetReadLimit(maxClientFrame)

	// base outlives the connection's life: cancelling a context under a read or
	// a write tears the socket down, and the end of a life must still send its
	// close frame. The request's own context is not for use after the upgrade.
	base, stop := context.WithCancel(context.WithoutCancel(r.Context()))
	defer stop()
	life, cancel := context.WithCancelCause(base)
	defer cancel(nil)
	if !s.track(&cancel) {
		_ = c.Close(websocket.StatusGoingAway, dropShutdown)
		return
	}
	defer s.untrack(&cancel)
	s.count(base, 1)
	defer s.count(base, -1)
	life, stopLife := context.WithTimeout(life, min(time.Until(p.Expiry), or(s.MaxLifetime, defaultMaxLifetime)))
	defer stopLife()

	v := &viewer{s: s, c: c, base: base, life: life, cancel: cancel, p: p, room: room, id: id}
	reason := v.serve()
	if reason != dropClientGone {
		s.dropped(base, reason)
	}
	switch reason {
	case dropReauth:
		_ = c.Close(closeReauth, reason)
	case dropShutdown:
		_ = c.Close(websocket.StatusGoingAway, reason)
	case dropSlowConsumer:
		_ = c.Close(websocket.StatusPolicyViolation, reason+": resume from afterSeq")
	case dropLogUnavailable:
		_ = c.Close(websocket.StatusTryAgainLater, reason)
	}
	// The reader returns once the socket is closed, the pinger with the life.
	_ = c.CloseNow()
	cancel(nil)
	v.wg.Wait()
}

// lookup reads the room and its log row before the upgrade, within routeTimeout,
// or writes the refusal.
func (s *Server) lookup(w http.ResponseWriter, r *http.Request, id string) (*v1alpha1.Room, store.RoomState, bool) {
	ctx, cancel := context.WithTimeout(r.Context(), routeTimeout)
	defer cancel()
	room, found, err := s.room(ctx, id)
	if err != nil {
		http.Error(w, "rooms unavailable", http.StatusServiceUnavailable)
		return nil, store.RoomState{}, false
	}
	if !found {
		http.Error(w, "no such room", http.StatusNotFound)
		return nil, store.RoomState{}, false
	}
	st, err := s.Log.Room(ctx, id)
	if errors.Is(err, store.ErrNoRoom) {
		http.Error(w, "no such room", http.StatusNotFound)
		return nil, store.RoomState{}, false
	}
	if err != nil {
		http.Error(w, "log unavailable", http.StatusServiceUnavailable)
		return nil, store.RoomState{}, false
	}
	return room, st, true
}

// viewer is one open WebSocket.
type viewer struct {
	s      *Server
	c      *websocket.Conn
	base   context.Context // I/O: ends with the handler
	life   context.Context // the connection's life: token, shutdown, liveness
	cancel context.CancelCauseFunc
	p      authn.Principal
	room   *v1alpha1.Room
	id     string
	last   int64          // the last seq written
	wg     sync.WaitGroup // the hello's read, the reader and the pinger
}

// write sends one frame within WriteWait. A peer that does not take it in time
// has stopped reading: errWriteTimeout, and the socket is gone.
func (v *viewer) write(f wire.ServerFrame) error {
	ctx, cancel := context.WithTimeout(v.base, or(v.s.WriteWait, defaultWriteWait))
	defer cancel()
	err := wsjson.Write(ctx, v.c, f)
	if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return errors.Join(errWriteTimeout, err)
	}
	return err
}

// serve runs the connection and returns why it ended, a drop reason. For
// protocol, the socket is already closed.
func (v *viewer) serve() string {
	// Read under base, like every read: a life that ends while the hello is awaited
	// still sends its close frame (1001 at shutdown, 4001 at the token's end).
	hctx, hcancel := context.WithTimeout(v.base, or(v.s.HelloWait, defaultHelloWait))
	var hello wire.ClientFrame
	got := make(chan error, 1)
	v.wg.Go(func() {
		defer hcancel()
		got <- wsjson.Read(hctx, v.c, &hello)
	})
	var err error
	select {
	case <-v.life.Done():
		return v.ended()
	case err = <-got:
	}
	if err != nil {
		// No hello within HelloWait tore the socket down: the broker's drop, not the peer's.
		if refused(err) || errors.Is(hctx.Err(), context.DeadlineExceeded) && v.life.Err() == nil {
			return dropProtocol
		}
		return v.ended()
	}
	if hello.Type != wire.FrameHello || hello.RoomID != v.id {
		_ = v.c.Close(websocket.StatusPolicyViolation, "hello first")
		return dropProtocol
	}
	feed, err := v.s.Hub.Subscribe(v.life, v.id)
	if err != nil {
		return v.failed(errors.Join(errLog, err))
	}
	defer v.s.Hub.Unsubscribe(feed)
	// The mark is read AFTER subscribing: everything above it arrives through the hub.
	st, err := v.s.Log.Room(v.life, v.id)
	if err != nil {
		return v.failed(errors.Join(errLog, err))
	}
	_, you := v.s.you(v.room, v.p, st.Driver)
	if err := v.write(wire.ServerFrame{Type: wire.FrameState, ThroughSeq: st.LastSeq, Snapshot: v.snapshot(you, st)}); err != nil {
		return v.failed(err)
	}
	after := max(st.LastSeq-defaultTail, 0)
	if hello.Tail > 0 {
		after = max(st.LastSeq-int64(hello.Tail), 0)
	}
	if hello.AfterSeq != nil {
		// Past the mark is clamped to it: the sync's fromSeq resets the client's
		// baseline, rather than skip live events up to its bogus seq.
		after = min(max(*hello.AfterSeq, 0), st.LastSeq)
	}
	if err := v.sendRange(after, st.LastSeq); err != nil {
		return v.failed(err)
	}

	frames := make(chan wire.ClientFrame)
	v.wg.Go(func() { v.read(frames) })
	v.wg.Go(v.ping)
	for {
		select {
		case <-v.life.Done():
			return v.ended()
		case <-feed.Dropped:
			return dropSlowConsumer
		case ev := <-feed.C:
			if ev.Seq <= v.last {
				feed.Sent(ev)
				continue // at or below what was sent from the log
			}
			if ev.Seq > v.last+1 { // a gap in live seq triggers a range read (§4)
				if err := v.sendRange(v.last, ev.Seq-1); err != nil {
					feed.Sent(ev)
					return v.failed(err)
				}
			}
			err := v.write(wire.ServerFrame{Type: wire.FrameEvent, Event: &ev})
			feed.Sent(ev) // only now: the budget covers what the socket has not taken
			if err != nil {
				return v.failed(err)
			}
			v.last = ev.Seq
			v.s.lag(v.base, ev)
		case f := <-frames:
			if f.Type != wire.FrameAct {
				continue // ping, or a repeated hello
			}
			ack := wire.ServerFrame{Type: wire.FrameAck, ClientSeq: f.ClientSeq, Rejected: wire.ReasonNotPermitted}
			if v.s.Acts != nil {
				ack = v.s.Acts(v.life, v.p, v.room, f)
			}
			if err := v.write(ack); err != nil {
				return v.failed(err)
			}
		}
	}
}

// failed names why an operation failed: the log or a write deadline while the
// connection was alive, or else the connection's end.
func (v *viewer) failed(err error) string {
	alive := v.life.Err() == nil
	switch {
	case errors.Is(err, errLog) && alive:
		return dropLogUnavailable
	// The timed-out write tore the socket down, and the reader may already have
	// reported that as the peer leaving: that end is the broker's, not the peer's.
	case errors.Is(err, errWriteTimeout) && (alive || errors.Is(context.Cause(v.life), errClientGone)):
		return dropWriteTimeout
	}
	return v.ended()
}

// ended names why the connection's life ended: its cause, or the token's deadline.
func (v *viewer) ended() string {
	cause := context.Cause(v.life)
	if errors.Is(cause, context.DeadlineExceeded) {
		return dropReauth
	}
	for _, e := range []error{errShutdown, errPingTimeout, errSlowConsumer, errProtocol} {
		if errors.Is(cause, e) {
			return e.Error()
		}
	}
	return dropClientGone // the peer left, or an I/O error while still alive
}

// read hands the client's frames to serve until the socket fails.
func (v *viewer) read(frames chan<- wire.ClientFrame) {
	for {
		var f wire.ClientFrame
		if err := wsjson.Read(v.base, v.c, &f); err != nil {
			if refused(err) {
				v.cancel(errProtocol)
			} else {
				v.cancel(errClientGone)
			}
			return
		}
		select {
		case frames <- f:
		case <-v.life.Done():
			return
		}
	}
}

// refused reports a client frame coder/websocket closed the socket over: past the
// read limit (1009) or not JSON a client frame decodes from (1007).
func refused(err error) bool {
	var syntax *json.SyntaxError
	var typ *json.UnmarshalTypeError
	return errors.Is(err, websocket.ErrMessageTooBig) || errors.As(err, &syntax) || errors.As(err, &typ)
}

// ping checks the peer every PingEvery; no pong within PongWait ends the connection.
func (v *viewer) ping() {
	t := time.NewTicker(or(v.s.PingEvery, defaultPingEvery))
	defer t.Stop()
	for {
		select {
		case <-v.life.Done():
			return
		case <-t.C:
		}
		ctx, cancel := context.WithTimeout(v.base, or(v.s.PongWait, defaultPongWait))
		err := v.c.Ping(ctx)
		cancel()
		if err != nil {
			v.cancel(errPingTimeout)
			return
		}
	}
}

// sendRange writes a sync frame, always (the client takes its seq baseline from
// the first one), then the events in (after, through], paged.
func (v *viewer) sendRange(after, through int64) error {
	v.last = after
	if err := v.write(wire.ServerFrame{Type: wire.FrameSync, FromSeq: after + 1, ThroughSeq: max(through, after)}); err != nil {
		return err
	}
	for v.last < through {
		evs, err := v.s.Log.Range(v.life, v.id, v.last, int(min(through-v.last, pageSize)))
		if err != nil {
			v.s.log().Warn("replay read failed", "room", v.id, "afterSeq", v.last, "err", err)
			return errors.Join(errLog, err)
		}
		if len(evs) == 0 {
			return nil
		}
		for i := range evs {
			if err := v.write(wire.ServerFrame{Type: wire.FrameEvent, Event: &evs[i]}); err != nil {
				return err
			}
			v.last = evs[i].Seq
		}
	}
	return nil
}

func (v *viewer) snapshot(you wire.You, st store.RoomState) *wire.Snapshot {
	snap := &wire.Snapshot{RoomID: v.id, Phase: v.room.Status.Phase, Driver: st.Driver, DriverEpoch: st.DriverEpoch,
		DataClass: v.room.Spec.DataClass, You: you, Runs: []wire.RunView{}}
	for _, run := range v.s.Runs.InRoom(v.id) {
		snap.Runs = append(snap.Runs, wire.RunView{ID: run.ID, Role: run.Role, Phase: run.Phase})
	}
	slices.SortFunc(snap.Runs, func(a, b wire.RunView) int { return strings.Compare(a.ID, b.ID) })
	return snap
}
