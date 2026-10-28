// SPDX-License-Identifier: Apache-2.0

package bridgeapi

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/Smana/maku/internal/envelope"
	"github.com/Smana/maku/internal/wire"
)

// stream is the bridge's one downstream channel (C4 r5: SSE, sandbox-initiated).
// It replays what the run has not acknowledged, from the log, then follows the
// room's hub: a delivery is derived from the log, so it survives a broker
// restart, a replica change and a bridge restart alike (§2 Steering).
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	p, run, ok := s.bridgeAuth(w, r)
	if !ok {
		return
	}
	if s.Hub == nil || s.LastAck == nil {
		fail(w, http.StatusInternalServerError, wire.ReasonStreamingFailed)
		return
	}
	rc := http.NewResponseController(w)
	ctx, done := s.conns.attach(r.Context(), run.ID)
	defer done()
	// The run may have ended between bridgeAuth and attach, and its Drop found no
	// stream to end: check again now that a Drop would find this one.
	if _, live, _ := s.Watch.Live(run.ID); !live {
		fail(w, http.StatusForbidden, wire.ReasonRunNotLive)
		return
	}
	// min(token exp, 1 h) bounds the stream (§4); the bridge re-dials with a fresh token.
	ctx, stop := context.WithTimeout(ctx, maxStreamLife)
	defer stop()
	if !p.Expiry.IsZero() {
		var stopAtExpiry context.CancelFunc
		ctx, stopAtExpiry = context.WithDeadline(ctx, p.Expiry)
		defer stopAtExpiry()
	}
	sub, err := s.Hub.Subscribe(ctx, run.Room)
	if err != nil {
		s.logFailure(w, err, "subscribe a stream to its room", "room", run.Room, "run", run.ID)
		return
	}
	defer s.Hub.Unsubscribe(sub)
	// The mark is read after subscribing, as for viewers (§4 Replay): every event
	// past it arrives through the hub. A failed read refuses the stream rather
	// than replay from 0, which would hand the harness every delivery again.
	after, err := s.LastAck(ctx, run.Room, run.ID)
	if err != nil {
		s.logFailure(w, err, "read a run's last acknowledgement", "room", run.Room, "run", run.ID)
		return
	}
	st, err := s.Log.Room(ctx, run.Room)
	if err != nil {
		s.logFailure(w, err, "read a stream's mark", "room", run.Room, "run", run.ID)
		return
	}
	every := s.PingEvery
	if every <= 0 {
		every = defaultPing
	}
	tick, stopTick := s.ticker(every)
	defer stopTick()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	write := func(frame string) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(s.streamWriteWait()))
		_, err := io.WriteString(w, frame)
		return err == nil
	}
	send := func(ev envelope.Event) bool {
		after = ev.Seq
		if name, data, ok := Deliverable(ev, run.ID); ok {
			return write("event: " + name + "\ndata: " + string(data) + "\n\n")
		}
		return true
	}
	// What the run has not acknowledged goes out first, in the stream's first
	// flush: a bridge that connects or resumes gets its pending deliveries before
	// anything else (review M15).
	for after < st.LastSeq {
		evs, err := s.Log.Deliveries(ctx, run.Room, run.ID, after, st.LastSeq, s.replayPage())
		if err != nil {
			s.log().Warn("replay a stream", "room", run.Room, "run", run.ID, errAttr(err))
			return // the bridge re-dials and resumes from its last ack
		}
		for _, ev := range evs {
			if !send(ev) {
				return
			}
		}
		if len(evs) < s.replayPage() {
			after = st.LastSeq // the replay covered up to the mark: the hub carries the rest
		}
	}
	ping := true // the first flush carries the replay, and a ping
	for {
		if ping && !write(": ping\n\n") {
			return
		}
		if err := rc.Flush(); err != nil {
			return
		}
		// Disarmed between writes (F10): Go's HTTP/2 server runs the deadline as a
		// per-stream timer and resets the stream when it fires, even with no write
		// pending, so an armed deadline would end every stream idle past it.
		_ = rc.SetWriteDeadline(time.Time{})
		select {
		case <-ctx.Done():
			return
		case <-sub.Dropped:
			return // the bridge re-dials and resumes from its last ack
		case ev := <-sub.C:
			ping = false
			// At or below the mark, the replay sent it.
			ok := ev.Seq <= after || send(ev)
			sub.Sent(ev) // only now, as the WebSocket: the budget covers what the bridge has not taken
			if !ok {
				return
			}
		case <-tick:
			ping = true
		}
	}
}

func (s *Server) streamWriteWait() time.Duration {
	if s.StreamWriteWait > 0 {
		return s.StreamWriteWait
	}
	return defaultStreamWriteWait
}

func (s *Server) ticker(d time.Duration) (<-chan time.Time, func()) {
	if s.Ticker != nil {
		return s.Ticker(d)
	}
	t := time.NewTicker(d)
	return t.C, t.Stop
}

// replayPage is the replay's read size: ReplayPage, or the default when unset.
func (s *Server) replayPage() int {
	if s.ReplayPage > 0 {
		return s.ReplayPage
	}
	return defaultReplayPage
}
