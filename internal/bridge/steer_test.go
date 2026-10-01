// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Smana/agent-platform/internal/wire"
)

func TestSteeringReachesTheHarnessOnceAndIsAcknowledged(t *testing.T) {
	f := &fakeAgentServer{pageSize: 100, status: "running"}
	var acks []wire.Item
	s := &Steering{Harness: NewHarness(f.start(t, conv).URL, conv), RunID: "7f3cq2xz", Push: func(it wire.Item) { acks = append(acks, it) }}
	_ = s.Deliver(context.Background(), wire.Deliver{Ref: 12, Text: "use v2"})
	_ = s.Deliver(context.Background(), wire.Deliver{Ref: 12, Text: "use v2"}) // a re-dialled stream replays it
	_ = s.Interrupt(context.Background(), wire.Interrupt{Ref: 13})
	if sent, _, _ := f.snapshot(); len(sent) != 1 || sent[0] != "use v2" {
		t.Fatalf("sent = %v", sent)
	}
	if len(acks) != 2 || !strings.Contains(string(acks[0].Payload), `"kind":"delivered"`) || !strings.Contains(string(acks[1].Payload), `"kind":"interrupted"`) {
		t.Fatalf("acks = %v", acks)
	}
	if acks[0].Stream != wire.StreamStatus || acks[0].Seq != 0 || string(acks[0].Payload) != `{"kind":"delivered","ref":12,"runId":"7f3cq2xz"}` {
		t.Fatalf("the ack goes on the status stream, numbered by the bridge: %+v %s", acks[0], acks[0].Payload)
	}
}

// A failed injection is not acknowledged, and its error ends the stream, so the
// replay hands the same ref over again before any later one.
func TestAFailedInjectionIsRetried(t *testing.T) {
	f := &fakeAgentServer{pageSize: 100, status: "running"}
	up := NewHarness(f.start(t, conv).URL, conv)
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	defer down.Close()
	var acks []wire.Item
	s := &Steering{Harness: NewHarness(down.URL, conv), RunID: "7f3cq2xz", Push: func(it wire.Item) { acks = append(acks, it) }}
	if err := s.Deliver(t.Context(), wire.Deliver{Ref: 12, Text: "use v2"}); err == nil {
		t.Fatal("a failed send must end the stream")
	}
	if err := s.Interrupt(t.Context(), wire.Interrupt{Ref: 13}); err == nil {
		t.Fatal("a failed interrupt must end the stream")
	}
	if len(acks) != 0 {
		t.Fatalf("acknowledged what never reached the harness: %v", acks)
	}
	s.Harness = up
	if s.Deliver(t.Context(), wire.Deliver{Ref: 12, Text: "use v2"}) != nil || s.Interrupt(t.Context(), wire.Interrupt{Ref: 13}) != nil {
		t.Fatal("the replay is handed over")
	}
	if sent, _, _ := f.snapshot(); len(sent) != 1 || len(acks) != 2 {
		t.Fatalf("sent %v, acks %v", sent, acks)
	}
}

// Review 4.3 I2: a permanent refusal is acknowledged as undeliverable, so the
// stream goes on and the next ref is delivered; a "not now" 4xx is retried.
func TestAPermanentRefusalIsUndeliverable(t *testing.T) {
	f := &fakeAgentServer{pageSize: 100, status: "running"}
	up := NewHarness(f.start(t, conv).URL, conv)
	code := http.StatusUnprocessableEntity
	refusing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }))
	defer refusing.Close()
	var acks []wire.Item
	s := &Steering{Harness: NewHarness(refusing.URL, conv), RunID: "7f3cq2xz", Push: func(it wire.Item) { acks = append(acks, it) }}
	if err := s.Deliver(t.Context(), wire.Deliver{Ref: 12, Text: "refused"}); err != nil {
		t.Fatalf("a permanent refusal must not end the stream: %v", err)
	}
	if err := s.Interrupt(t.Context(), wire.Interrupt{Ref: 13}); err != nil {
		t.Fatalf("a permanent refusal must not end the stream: %v", err)
	}
	if len(acks) != 2 || string(acks[0].Payload) != `{"code":422,"kind":"undeliverable","ref":12,"runId":"7f3cq2xz"}` ||
		!strings.Contains(string(acks[1].Payload), `"kind":"undeliverable","ref":13`) {
		t.Fatalf("acks = %v", acks)
	}
	if s.Deliver(t.Context(), wire.Deliver{Ref: 12, Text: "refused"}) != nil || len(acks) != 2 {
		t.Fatal("an undeliverable ref is settled: a replay of it is skipped")
	}
	code = http.StatusNotFound // the conversation does not exist yet: not now
	if err := s.Deliver(t.Context(), wire.Deliver{Ref: 14, Text: "early"}); err == nil || len(acks) != 2 {
		t.Fatalf("a 404 is retried, not settled: %v %v", err, acks)
	}
	s.Harness = up
	if s.Deliver(t.Context(), wire.Deliver{Ref: 14, Text: "early"}) != nil || len(acks) != 3 ||
		!strings.Contains(string(acks[2].Payload), `"kind":"delivered","ref":14`) {
		t.Fatalf("the next ref is delivered: %v", acks)
	}
}

// Push is safe from any goroutine; the loop numbers status items once, after
// the stream's cursor at hello.
func TestPushIsGoroutineSafeAndNumberedOnce(t *testing.T) {
	b := &Bridge{RunID: runID}
	if err := b.init(); err != nil {
		t.Fatal(err)
	}
	b.status.Resume(40)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			for k := range 25 {
				b.Push(wire.Item{Stream: wire.StreamStatus, Type: "state_changed",
					Payload: []byte(fmt.Sprintf(`{"kind":"delivered","ref":%d}`, i*100+k))})
			}
		})
	}
	wg.Wait()
	b.takeInbox(t.Context())
	seen := map[int64]bool{}
	for _, p := range b.buf {
		if p.stream != wire.StreamStatus || p.seq <= 40 || seen[p.seq] {
			t.Fatalf("item %s %d", p.stream, p.seq)
		}
		seen[p.seq] = true
	}
	if len(seen) != 200 || !seen[41] || !seen[240] {
		t.Fatalf("%d items numbered", len(seen))
	}
}

// End to end: a deliver frame reaches the harness, and its acknowledgement
// reaches the broker on the status stream.
func TestADeliveryIsAcknowledgedInTheLog(t *testing.T) {
	f := &fakeAgentServer{pageSize: 100, status: "idle"}
	fb := &fakeBroker{resume: wire.Resume{AfterStatusSeq: 9}, sse: []string{": ping\n\n", "event: deliver\ndata: {\"ref\":12,\"text\":\"use v2\"}\n\n"}}
	r := newRig(t, NewHarness(f.start(t, conv).URL, conv), fb)
	steer := &Steering{Harness: r.b.Harness, RunID: runID, Push: r.b.Push}
	r.b.OnDeliver = steer.Deliver
	_, stop := r.run(t)
	defer stop()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, it := range fb.stored(wire.StreamStatus) {
			if strings.Contains(string(it.Payload), `"kind":"delivered"`) {
				if it.Seq <= 9 {
					t.Fatalf("the ack reuses a status seq the log holds: %d", it.Seq)
				}
				if sent, _, _ := f.snapshot(); len(sent) != 1 || sent[0] != "use v2" {
					t.Fatalf("sent %v", sent)
				}
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the delivery was never acknowledged")
}

// Review M15: an event's data lines are bounded together, not only one by one;
// and a hook's error ends the stream.
func TestStreamBoundsAnEventsData(t *testing.T) {
	line := "data: " + strings.Repeat("x", 64<<10) + "\n"
	for _, c := range []struct {
		name  string
		frame string
		hook  error
		want  string
	}{
		{"data past the cap", "event: deliver\n" + strings.Repeat(line, 5) + "\n", nil, "passes"},
		{"a failing hook", "event: deliver\ndata: {\"ref\":1,\"text\":\"x\"}\n\n", errors.New("harness down"), "harness down"},
	} {
		t.Run(c.name, func(t *testing.T) {
			fb := &fakeBroker{sse: []string{c.frame}}
			srv, ca := fb.start(t)
			br, err := NewBroker(srv.URL, writeToken(t, t.TempDir(), "v1"), ca)
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second) // a stream that reads on ends here, without error
			defer cancel()
			err = br.Stream(ctx, func(string, []byte) error { calls++; return c.hook })
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("Stream = %v", err)
			}
			if c.hook == nil && calls != 0 {
				t.Fatal("an oversize event was handed over")
			}
		})
	}
}
