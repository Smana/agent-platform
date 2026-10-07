// SPDX-License-Identifier: Apache-2.0

package bridgeapi

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Smana/agent-platform/internal/envelope"
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
	time.Sleep(100 * time.Millisecond) // the hub offers seq 1 to the buffer
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
	defer func(n int) { replayPage = n }(replayPage)
	replayPage = 2
	s, log, w := newServer(t)
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
