// SPDX-License-Identifier: Apache-2.0

package bridgeapi

import (
	"bufio"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Smana/agent-platform/internal/envelope"
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
