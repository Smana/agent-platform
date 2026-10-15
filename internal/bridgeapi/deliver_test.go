// SPDX-License-Identifier: Apache-2.0

package bridgeapi

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/fanout"
	"github.com/Smana/agent-platform/internal/store"
	"github.com/Smana/agent-platform/internal/wire"
)

func TestDeliverable(t *testing.T) {
	steer := envelope.Event{Seq: 12, Type: envelope.Message, Payload: envelope.Must(envelope.MessagePayload{
		Kind: envelope.KindChat, Text: "use v2", To: []string{"agent:7f3cq2xz"}, Delivery: envelope.DeliverySteering})}
	if ev, data, ok := Deliverable(steer, "7f3cq2xz"); !ok || ev != "deliver" || string(data) != `{"ref":12,"text":"use v2"}` {
		t.Fatalf("%s %s %v", ev, data, ok)
	}
	if _, _, ok := Deliverable(steer, "aaaaaaaa"); ok {
		t.Fatal("steering for another run")
	}
	queued := steer
	queued.Payload = envelope.Must(envelope.MessagePayload{Kind: envelope.KindChat, Text: "later", Delivery: envelope.DeliveryQueued})
	if _, _, ok := Deliverable(queued, "7f3cq2xz"); ok {
		t.Fatal("a queued message waits for the next brief")
	}
	for _, d := range []envelope.Delivery{envelope.DeliveryQueued, envelope.DeliveryNone} { // addressed, yet not steering
		addressed := steer
		addressed.Payload = envelope.Must(envelope.MessagePayload{Kind: envelope.KindChat, Text: "x", To: []string{"agent:7f3cq2xz"}, Delivery: d})
		if _, _, ok := Deliverable(addressed, "7f3cq2xz"); ok {
			t.Fatalf("a %s message addressed to the run", d)
		}
	}
	intr := envelope.Event{Seq: 13, Type: envelope.StateChanged, Payload: envelope.StatePayload("interrupt", map[string]any{"runId": "7f3cq2xz"})}
	if ev, _, ok := Deliverable(intr, "7f3cq2xz"); !ok || ev != "interrupt" {
		t.Fatal("interrupt")
	}
	other := intr
	other.Payload = envelope.StatePayload("interrupt", map[string]any{"runId": "aaaaaaaa"})
	if _, _, ok := Deliverable(other, "7f3cq2xz"); ok {
		t.Fatal("another run's interrupt")
	}
}

// human appends, as a human's act does, an event of t to the room.
func human(t *testing.T, log *memLog, n int64, typ envelope.Type, payload []byte) {
	t.Helper()
	if _, _, err := log.Append(t.Context(), envelope.Draft{RoomID: room, Actor: envelope.Actor{Kind: envelope.ActorHuman, ID: "human:own"},
		Type: typ, Origin: envelope.OriginClient, OriginClient: "human:own:s1", OriginSeq: n, Payload: payload}); err != nil {
		t.Fatal(err)
	}
}

func steering(text, run string) []byte {
	return envelope.Must(envelope.MessagePayload{Kind: envelope.KindChat, Text: text, To: []string{"agent:" + run},
		Delivery: envelope.DeliverySteering})
}

// expectFrame reads one SSE frame and checks its event and data lines.
func expectFrame(t *testing.T, sc *bufio.Scanner, event, data string) {
	t.Helper()
	for _, want := range []string{"event: " + event, "data: " + data, ""} {
		if !sc.Scan() || sc.Text() != want {
			t.Fatalf("want %q, got %q (%v)", want, sc.Text(), sc.Err())
		}
	}
}

// Review M15, §2: a bridge that connects or resumes gets what its run has not
// acknowledged first, with data, from the log: an acknowledged delivery, another
// run's steering, chat and a queued message are not sent.
func TestStreamReplaysWhatTheRunHasNotAcknowledged(t *testing.T) {
	s, log, w := newServer(t)
	s.Ticker = make(manualTicker).new
	w.Upsert(t.Context(), agentRun(runA, room, "Running"))
	human(t, log, 1, envelope.Message, steering("acknowledged", runA))
	if _, _, err := log.Append(t.Context(), envelope.Draft{RoomID: room, RunID: runA,
		Actor: envelope.Actor{Kind: envelope.ActorAgent, ID: "agent:" + runA}, Type: envelope.StateChanged,
		Origin: envelope.OriginHarness, OriginClient: "agent:" + runA + ":status", OriginSeq: 1,
		Payload: envelope.StatePayload("delivered", map[string]any{"ref": 1, "runId": runA})}); err != nil {
		t.Fatal(err)
	}
	human(t, log, 2, envelope.Message, steering("not for us", "aaaaaaaa"))
	human(t, log, 3, envelope.Message, envelope.Must(envelope.MessagePayload{Kind: envelope.KindChat, Text: "later",
		Delivery: envelope.DeliveryQueued}))
	human(t, log, 4, envelope.Message, steering("use v2", runA))
	human(t, log, 5, envelope.StateChanged, envelope.StatePayload("interrupt", map[string]any{"runId": runA}))
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	sc, stop := openStream(t, srv, runA)
	defer stop()
	defer time.AfterFunc(5*time.Second, stop).Stop()
	expectFrame(t, sc, wire.EventDeliver, `{"ref":5,"text":"use v2"}`)
	expectFrame(t, sc, wire.EventInterrupt, `{"ref":6}`)
	expectPing(t, sc)
}

// After the replay, the stream follows the room's live events.
func TestStreamFollowsTheRoom(t *testing.T) {
	s, log, w := newServer(t)
	s.Ticker = make(manualTicker).new
	w.Upsert(t.Context(), agentRun(runA, room, "Running"))
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	sc, stop := openStream(t, srv, runA)
	defer stop()
	defer time.AfterFunc(5*time.Second, stop).Stop()
	expectPing(t, sc)
	human(t, log, 1, envelope.Message, steering("now", runA))
	expectFrame(t, sc, wire.EventDeliver, `{"ref":1,"text":"now"}`)
}

// A stream that cannot know where its run is refuses, rather than replay every
// delivery from the start.
func TestStreamRefusesWithoutItsMark(t *testing.T) {
	s, _, w := newServer(t)
	w.Upsert(t.Context(), agentRun(runA, room, "Running"))
	s.LastAck = func(context.Context, string, string) (int64, error) { return 0, errors.New("down") }
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second) // a stream that opens anyway ends here
	defer cancel()
	r := httptest.NewRequestWithContext(ctx, http.MethodGet, "/v1/bridge/stream", nil)
	r.Header.Set("Authorization", "Bearer run:"+runA)
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, r)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("an unreadable ack: %d", rec.Code)
	}
	s.LastAck, s.Hub = nil, nil
	if rec := call(t, s.Routes(), http.MethodGet, "/v1/bridge/stream", "run:"+runA, nil); rec.Code != http.StatusInternalServerError ||
		reason(t, rec) != wire.ReasonStreamingFailed {
		t.Fatalf("no hub: %d", rec.Code)
	}
}

// Review 4.3 I1: an event appended between Subscribe and the mark is in the hub's
// buffer and in the replay: it goes out once.
func TestStreamSendsTheSeamOnce(t *testing.T) {
	s, log, w := newServer(t)
	s.Ticker = make(manualTicker).new
	log.mu.Lock()
	log.ranged = make(chan int64, 16) // the hub's poll already runs
	log.mu.Unlock()
	w.Upsert(t.Context(), agentRun(runA, room, "Running"))
	var once sync.Once
	s.LastAck = func(ctx context.Context, r, run string) (int64, error) {
		once.Do(func() { human(t, log, 1, envelope.Message, steering("seam", runA)) })
		return log.LastAck(ctx, r, run)
	}
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	sc, stop := openStream(t, srv, runA)
	defer stop()
	defer time.AfterFunc(5*time.Second, stop).Stop()
	expectFrame(t, sc, wire.EventDeliver, `{"ref":1,"text":"seam"}`)
	expectPing(t, sc)
	for seen := int64(0); seen < 1; { // the hub has read seq 1 and offers it to the buffer
		select {
		case seen = <-log.ranged:
		case <-time.After(2 * time.Second):
			t.Fatal("the hub never read seq 1")
		}
	}
	human(t, log, 2, envelope.Message, steering("next", runA))
	expectFrame(t, sc, wire.EventDeliver, `{"ref":2,"text":"next"}`)
}

// roomHook appends once, right after the stream reads its mark; the hub reads
// the memLog directly.
type roomHook struct {
	*memLog
	once  sync.Once
	after func()
}

func (h *roomHook) Room(ctx context.Context, id string) (store.RoomState, error) {
	st, err := h.memLog.Room(ctx, id)
	h.once.Do(h.after)
	return st, err
}

// Review 4.3 I1: an event appended just after the mark is past the replay, so the
// subscription must predate the mark.
func TestStreamMissesNothingAfterTheMark(t *testing.T) {
	s, log, w := newServer(t)
	s.Ticker = make(manualTicker).new
	w.Upsert(t.Context(), agentRun(runA, room, "Running"))
	s.Log = &roomHook{memLog: log, after: func() { human(t, log, 1, envelope.Message, steering("gap", runA)) }}
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	sc, stop := openStream(t, srv, runA)
	defer stop()
	defer time.AfterFunc(3*time.Second, stop).Stop()
	expectPing(t, sc)
	expectFrame(t, sc, wire.EventDeliver, `{"ref":1,"text":"gap"}`)
}

// Review 4.3 I1, I3: the replay pages through the run's deliveries, all of them,
// in as many reads as pages, never the room's whole log.
func TestStreamReplayPages(t *testing.T) {
	s, log, w := newServer(t)
	s.ReplayPage = 2
	s.Ticker = make(manualTicker).new
	w.Upsert(t.Context(), agentRun(runA, room, "Running"))
	for i := range int64(5) {
		human(t, log, 2*i+1, envelope.Message, steering("m", runA))
		human(t, log, 2*i+2, envelope.Message, envelope.Must(envelope.MessagePayload{Kind: envelope.KindChat, Text: "chat",
			Delivery: envelope.DeliveryNone}))
	}
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	sc, stop := openStream(t, srv, runA)
	defer stop()
	defer time.AfterFunc(5*time.Second, stop).Stop()
	for _, ref := range []string{"1", "3", "5", "7", "9"} {
		expectFrame(t, sc, wire.EventDeliver, `{"ref":`+ref+`,"text":"m"}`)
	}
	expectPing(t, sc)
	log.mu.Lock()
	reads := log.deliveryReads
	log.mu.Unlock()
	if reads != 3 {
		t.Fatalf("the replay read the log %d times, want 3 pages of 2", reads)
	}
}

// blockingWriter is a stream's response writer whose event frames block until
// released: a bridge that stopped reading mid-frame.
type blockingWriter struct {
	header  http.Header
	pinged  chan struct{} // capacity 1: a ping was written
	writing chan string   // each event frame as its write starts
	release chan struct{} // closed: event frames complete
}

func (b *blockingWriter) Header() http.Header { return b.header }
func (b *blockingWriter) WriteHeader(int)     {}
func (b *blockingWriter) Flush()              {}
func (b *blockingWriter) Write(p []byte) (int, error) {
	if strings.HasPrefix(string(p), "event:") {
		b.writing <- string(p)
		<-b.release
		return len(p), nil
	}
	select {
	case b.pinged <- struct{}{}:
	default:
	}
	return len(p), nil
}

// Like the WebSocket, the stream frees an event's share of the hub's budget only
// once the write took it: a bridge stalled on a write is a slow consumer.
func TestStreamHoldsTheBudgetUntilTheWrite(t *testing.T) {
	s, log, w := newServer(t)
	s.Ticker = make(manualTicker).new
	w.Upsert(t.Context(), agentRun(runA, room, "Running"))
	hub, ok := s.Hub.(*fanout.Hub)
	if !ok {
		t.Fatalf("hub %T", s.Hub)
	}
	witness, err := hub.Subscribe(t.Context(), room) // at the default budget
	if err != nil {
		t.Fatal(err)
	}
	defer hub.Unsubscribe(witness)
	one := len(steering("a", runA))
	hub.Budget = one + one/2     // the stream's: one event fits, two do not
	offered := func(seq int64) { // the hub's pass offered seq to every subscriber
		t.Helper()
		for got := int64(0); got < seq; {
			select {
			case ev := <-witness.C:
				witness.Sent(ev)
				got = ev.Seq
			case <-time.After(5 * time.Second):
				t.Fatalf("seq %d never offered", seq)
			}
		}
		probe, err := hub.Subscribe(t.Context(), room) // waits out the pass's lock
		if err != nil {
			t.Fatal(err)
		}
		hub.Unsubscribe(probe)
	}
	bw := &blockingWriter{header: http.Header{}, pinged: make(chan struct{}, 1), writing: make(chan string, 2), release: make(chan struct{})}
	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/v1/bridge/stream", nil)
	req.Header.Set("Authorization", "Bearer run:"+runA)
	served := make(chan struct{})
	go func() { s.Routes().ServeHTTP(bw, req); close(served) }()
	select {
	case <-bw.pinged: // subscribed and past the replay: event 1 comes from the hub
	case <-time.After(5 * time.Second):
		t.Fatal("the stream never opened")
	}
	human(t, log, 1, envelope.Message, steering("a", runA))
	select {
	case <-bw.writing: // the stream holds event 1, mid-write
	case <-time.After(5 * time.Second):
		t.Fatal("event 1 never written")
	}
	human(t, log, 2, envelope.Message, steering("b", runA))
	offered(2)
	close(bw.release)
	select {
	case <-served:
	case f := <-bw.writing:
		t.Fatalf("the stream wrote %q past its budget", f)
	case <-time.After(5 * time.Second):
		t.Fatal("the stream outlived its budget")
	}
}
