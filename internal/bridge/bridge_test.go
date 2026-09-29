// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/wire"
)

func chatEvent(text string) map[string]any {
	return map[string]any{"kind": "MessageEvent", "source": "agent",
		"llm_message": map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}}}
}

// seqs lists the seqs of items, failing the test on a duplicate. The fake
// drops a resent seq as the store does, so a duplicate here is a fake bug.
func seqs(t *testing.T, items []wire.Item) []int64 {
	t.Helper()
	out := make([]int64, 0, len(items))
	for _, it := range items {
		if slices.Contains(out, it.Seq) {
			t.Fatalf("seq %d (%s) uploaded twice", it.Seq, it.Stream)
		}
		out = append(out, it.Seq)
	}
	return out
}

func TestBridgeMirrorsEverythingOnceAndSurvivesABrokerOutage(t *testing.T) {
	f := &fakeAgentServer{pageSize: 2, status: "running"}
	for i := range 5 {
		f.add(chatEvent(fmt.Sprint("hi ", i)))
	}
	fb := &fakeBroker{events: []reply{{code: http.StatusServiceUnavailable}, {code: http.StatusServiceUnavailable}}}
	r := newRig(t, NewHarness(f.start(t, conv).URL, conv), fb)
	ctx, stop := r.run(t)
	eventually(ctx, t, "five events are mirrored", func() bool { return len(fb.stored(wire.StreamEvents)) == 5 })
	writeToken(t, r.tokDir, "v2") // kubelet's rotation: a plain re-read sees it
	f.mu.Lock()
	f.status = "finished"
	f.mu.Unlock()
	eventually(ctx, t, "the turn completes", func() bool {
		return slices.ContainsFunc(fb.stored(wire.StreamStatus), func(it wire.Item) bool { return payloadHas(it, `"completed"`) })
	})
	stop()

	if got := seqs(t, fb.stored(wire.StreamEvents)); !slices.Equal(got, []int64{SeqFor(1, 0), SeqFor(2, 0), SeqFor(3, 0), SeqFor(4, 0), SeqFor(5, 0)}) {
		t.Fatalf("event seqs = %v", got)
	}
	seqs(t, fb.stored(wire.StreamStatus))
	fb.mu.Lock()
	defer fb.mu.Unlock()
	if last := fb.tokens[len(fb.tokens)-1]; last != "Bearer v2" {
		t.Fatalf("the token was not re-read: %s", last)
	}
}

func TestBridgeResumesWhereTheLogIs(t *testing.T) {
	f := &fakeAgentServer{pageSize: 100, status: "running"}
	for range 4 {
		f.add(map[string]any{"kind": "MessageEvent", "source": "agent"})
	}
	fb := &fakeBroker{resume: wire.Resume{RoomID: "3kq7x2ma", AfterHarnessSeq: SeqFor(3, 0)}}
	r := newRig(t, NewHarness(f.start(t, conv).URL, conv), fb)
	ctx, stop := r.run(t)
	eventually(ctx, t, "event 4 is mirrored", func() bool { return len(fb.stored(wire.StreamEvents)) >= 2 })
	stop()
	// Event 3 is re-sent (the store drops it), event 4 is new; 1 and 2 are skipped.
	if got := seqs(t, fb.stored(wire.StreamEvents)); !slices.Equal(got, []int64{SeqFor(3, 0), SeqFor(4, 0)}) {
		t.Fatalf("seqs = %v", got)
	}
}

// Ruling AL (b, c): a restarted bridge rebuilds the three-field cursor with Skip,
// so malformed events at the log's tail are neither re-sent under new seqs nor
// skipped, and its turns continue the status stream's seqs instead of reusing ids.
func TestARestartedBridgeContinuesTheSameSeqsAndTurns(t *testing.T) {
	f := &fakeAgentServer{pageSize: 100, status: "running"}
	f.add(chatEvent("one"))
	f.addAsIs(map[string]any{"kind": "MessageEvent"}) // malformed: no id
	f.addAsIs(map[string]any{"kind": "MessageEvent"})
	harness := f.start(t, conv).URL

	first := &fakeBroker{}
	r1 := newRig(t, NewHarness(harness, conv), first)
	ctx, stop := r1.run(t)
	eventually(ctx, t, "the first bridge mirrors three events and a turn", func() bool {
		return len(first.stored(wire.StreamEvents)) == 3 && len(first.stored(wire.StreamStatus)) == 2
	})
	stop()
	before := first.stored(wire.StreamStatus)

	// The log's cursors, as the broker's hello reports them.
	resume := wire.Resume{RoomID: "3kq7x2ma"}
	for _, it := range first.stored(wire.StreamEvents) {
		resume.AfterHarnessSeq = max(resume.AfterHarnessSeq, it.Seq)
	}
	for _, it := range before {
		resume.AfterStatusSeq = max(resume.AfterStatusSeq, it.Seq)
	}
	f.add(chatEvent("four"))
	second := &fakeBroker{resume: resume}
	r2 := newRig(t, NewHarness(harness, conv), second)
	ctx, stop = r2.run(t)
	eventually(ctx, t, "the second bridge mirrors event four and its turn", func() bool {
		return slices.ContainsFunc(second.stored(wire.StreamEvents), func(it wire.Item) bool { return payloadHas(it, "four") }) &&
			len(second.stored(wire.StreamStatus)) == 2
	})
	stop()

	got := second.stored(wire.StreamEvents)
	if s := seqs(t, got); !slices.Equal(s, []int64{SeqFor(3, 0), SeqFor(4, 0)}) {
		t.Fatalf("after a restart the bridge sent seqs %v, want the log's last event again and event 4", s)
	}
	if !payloadHas(got[0], "malformed") || !payloadHas(got[1], "four") {
		t.Fatalf("items keyed to the wrong events: %s / %s", got[0].Payload, got[1].Payload)
	}
	turnID := func(items []wire.Item) string {
		for _, it := range items {
			var p envelope.TurnPayload
			if it.Type == envelope.Turn && json.Unmarshal(it.Payload, &p) == nil {
				return p.TurnID
			}
		}
		t.Fatal("no turn item")
		return ""
	}
	after := second.stored(wire.StreamStatus)
	if after[0].Seq != resume.AfterStatusSeq+1 {
		t.Fatalf("status seq restarted at %d, want %d", after[0].Seq, resume.AfterStatusSeq+1)
	}
	if a, b := turnID(before), turnID(after); a == b {
		t.Fatalf("both bridges named their turn %q", a)
	}
}

func TestBatchesAreCutByEncodedBytes(t *testing.T) {
	item := func(seq int64, payload string) pending {
		return encode(wire.Item{Stream: wire.StreamEvents, Seq: seq, Type: envelope.Message, Payload: json.RawMessage(payload)})
	}
	plain := `{"text":"` + strings.Repeat("a", 1000) + `"}`
	// < escapes to six bytes when the batch is marshalled: a raw-byte count
	// would put nine of these under a 10 000-byte cap.
	escaped := `{"text":"` + strings.Repeat("<", 1000) + `"}`
	cases := []struct {
		name     string
		buf      []pending
		maxBytes int
		maxItems int
		want     int
	}{
		{"everything fits", []pending{item(4, plain), item(8, plain)}, 10_000, 100, 2},
		{"the item cap binds", []pending{item(4, plain), item(8, plain), item(12, plain)}, 10_000, 2, 2},
		{"the byte cap binds", []pending{item(4, plain), item(8, plain), item(12, plain)}, 2_200, 100, 2},
		{"escaping counts", []pending{item(4, escaped), item(8, escaped)}, 10_000, 100, 1},
		{"a lone item over the cap is still one batch", []pending{item(4, escaped)}, 100, 100, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			n := nextBatch(c.buf, c.maxBytes, c.maxItems)
			if n != c.want {
				t.Fatalf("batch of %d, want %d", n, c.want)
			}
			if n > 1 && len(batchBody(c.buf[:n])) > c.maxBytes {
				t.Fatalf("a %d-item batch encodes to %d bytes, over %d", n, len(batchBody(c.buf[:n])), c.maxBytes)
			}
			var back wire.Batch
			if err := json.Unmarshal(batchBody(c.buf[:n]), &back); err != nil || len(back.Items) != n {
				t.Fatalf("the body is not a %d-item batch: %v", n, err)
			}
		})
	}
}

// A 413 halves the batch until it fits; a lone item still over the cap is
// replaced by a stub in its slot. Nothing is dropped.
func TestOversizeBatchesAreSplitNeverDropped(t *testing.T) {
	cases := []struct {
		name    string
		text    string
		maxBody int
		stubbed bool
	}{
		{"a batch over the broker's cap is halved until it fits", strings.Repeat("x", 3000), 8 << 10, false},
		{"a lone item over the broker's cap becomes a stub", strings.Repeat("x", 3000), 2 << 10, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeAgentServer{pageSize: 100, status: "running"}
			for range 6 {
				f.add(chatEvent(c.text))
			}
			fb := &fakeBroker{maxBody: c.maxBody}
			r := newRig(t, NewHarness(f.start(t, conv).URL, conv), fb)
			ctx, stop := r.run(t)
			eventually(ctx, t, "six events are stored", func() bool { return len(fb.stored(wire.StreamEvents)) == 6 })
			stop()
			got := fb.stored(wire.StreamEvents)
			if s := seqs(t, got); !slices.Equal(s, []int64{4, 8, 12, 16, 20, 24}) {
				t.Fatalf("seqs = %v", s)
			}
			if !slices.Contains(fb.callLog(), "events Request Entity Too Large") {
				t.Fatal("the broker never refused a batch: the case tests nothing")
			}
			for _, it := range got {
				if isStub := payloadHas(it, `"oversize"`); isStub != c.stubbed {
					t.Fatalf("stubbed = %v, want %v: %.120s", isStub, c.stubbed, it.Payload)
				}
			}
			if n := r.counter(t, metricStubbed, StubOversize); c.stubbed && n != 6 {
				t.Fatalf("%s{reason=oversize} = %d, want 6", metricStubbed, n)
			}
		})
	}
}

// An item the broker refuses for what it is (400) is found by halving and kept
// as a stub in its slot, so the cursor moves on and its neighbours land as sent.
func TestARefusedItemBecomesAStub(t *testing.T) {
	f := &fakeAgentServer{pageSize: 100, status: "running"}
	for _, s := range []string{"a", "b", "poison", "d"} {
		f.add(chatEvent(s))
	}
	fb := &fakeBroker{poison: "poison"}
	r := newRig(t, NewHarness(f.start(t, conv).URL, conv), fb)
	ctx, stop := r.run(t)
	eventually(ctx, t, "four events are stored", func() bool { return len(fb.stored(wire.StreamEvents)) == 4 })
	stop()
	got := fb.stored(wire.StreamEvents)
	if s := seqs(t, got); !slices.Equal(s, []int64{4, 8, 12, 16}) {
		t.Fatalf("seqs = %v", s)
	}
	for _, it := range got {
		switch {
		case it.Seq == 12 && (!payloadHas(it, `"refused"`) || payloadHas(it, "poison")):
			t.Fatalf("the refused item was not stubbed: %s", it.Payload)
		case it.Seq != 12 && it.Type != envelope.Message:
			t.Fatalf("a neighbour was stubbed: %s", it.Payload)
		}
	}
	if n := r.counter(t, metricStubbed, StubRefused); n != 1 {
		t.Fatalf("%s{reason=refused} = %d, want 1", metricStubbed, n)
	}
}

func TestRetryAfterIsHonoured(t *testing.T) {
	for _, code := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			t.Parallel()
			f := &fakeAgentServer{pageSize: 100, status: "running"}
			f.add(chatEvent("hi"))
			fb := &fakeBroker{events: []reply{{code: code, retryAfter: "1"}}}
			r := newRig(t, NewHarness(f.start(t, conv).URL, conv), fb)
			ctx, stop := r.run(t)
			eventually(ctx, t, "the event lands", func() bool { return len(fb.stored(wire.StreamEvents)) == 1 })
			stop()
			fb.mu.Lock()
			defer fb.mu.Unlock()
			if gap := fb.posts[1].Sub(fb.posts[0]); gap < time.Second {
				t.Fatalf("retried after %v, before the broker's Retry-After of 1 s", gap)
			}
		})
	}
}

func TestRetryDelay(t *testing.T) {
	now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		header string
		want   time.Duration
	}{
		{"seconds", "7", 7 * time.Second},
		{"an HTTP date", now.Add(4 * time.Second).Format(http.TimeFormat), 4 * time.Second},
		{"a date in the past means now", now.Add(-time.Minute).Format(http.TimeFormat), 0},
		{"a huge value is capped", "86400", maxRetryWait},
		{"absent falls back to the backoff", "", 250 * time.Millisecond},
		{"garbage falls back to the backoff", "soon", 250 * time.Millisecond},
		{"negative falls back to the backoff", "-3", 250 * time.Millisecond},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := retryDelay(c.header, now, 250*time.Millisecond); got != c.want {
				t.Fatalf("retryDelay(%q) = %v, want %v", c.header, got, c.want)
			}
		})
	}
}

// Ruling Y: a 409 lease_lost means another run holds the room. The bridge stops
// appending, keeps its buffer, and says hello until it holds the lease again.
func TestALostLeaseStopsAppendingUntilHelloSucceeds(t *testing.T) {
	f := &fakeAgentServer{pageSize: 100, status: "running"}
	for range 3 {
		f.add(chatEvent("hi"))
	}
	fb := &fakeBroker{
		events: []reply{{code: http.StatusConflict, reason: wire.ReasonLeaseLost}},
		// The first hello takes the lease; the first re-acquire finds the room busy.
		hellos: []reply{{code: http.StatusOK}, {code: http.StatusConflict, reason: wire.ReasonRoomBusy}},
	}
	r := newRig(t, NewHarness(f.start(t, conv).URL, conv), fb)
	ctx, stop := r.run(t)
	eventually(ctx, t, "three events land", func() bool { return len(fb.stored(wire.StreamEvents)) == 3 })
	stop()

	calls := fb.callLog()
	if !slices.Contains(calls, "hello Conflict") {
		t.Fatalf("the bridge never met the busy room: %v", calls)
	}
	lost := slices.Index(calls, "events Conflict")
	var after []string
	for _, c := range calls[lost+1:] {
		if c != "events Conflict" {
			after = append(after, c)
		}
	}
	// Nothing is appended between the 409 and a hello that took the lease back.
	firstOK := slices.Index(after, "hello OK")
	firstEvents := slices.IndexFunc(after, func(c string) bool { return strings.HasPrefix(c, "events") })
	if firstOK < 0 || firstEvents < firstOK {
		t.Fatalf("appended before re-acquiring the lease: %v", calls)
	}
	seqs(t, fb.stored(wire.StreamEvents))
	if !strings.Contains(r.logs.String(), "lease") {
		t.Fatal("the lost lease was not logged")
	}
}

// The adapter's three typed errors each hold the cursor where it is. The bridge
// counts and logs every one, says so once in the room, and keeps polling.
func TestAStalledHarnessIsLoudAndNeverSkipped(t *testing.T) {
	serve := func(t *testing.T, search http.HandlerFunc) *Harness {
		mux := http.NewServeMux()
		base := "/api/conversations/" + conv
		mux.HandleFunc("GET "+base, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"execution_status":"running"}`))
		})
		mux.HandleFunc("GET "+base+"/events/search", search)
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		return NewHarness(srv.URL, conv)
	}
	cases := []struct {
		name    string
		reason  string
		harness func(t *testing.T) *Harness
	}{
		{"an event over the response cap", StallEventTooLarge, func(t *testing.T) *Harness {
			f := &fakeAgentServer{pageSize: 100, status: "running"}
			f.add(chatEvent("e1"))
			f.add(chatEvent(strings.Repeat("x", 8<<10)))
			f.add(chatEvent("e3"))
			h := NewHarness(f.start(t, conv).URL, conv)
			h.maxBody = 4 << 10
			return h
		}},
		{"agent-server losing the cursor", StallCursorLost, func(t *testing.T) *Harness {
			return serve(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("page_id") == "" {
					_, _ = w.Write([]byte(`{"items":[{"id":"e1","kind":"PauseEvent"}],"next_page_id":null}`))
					return
				}
				_, _ = w.Write([]byte(`{"items":[{"id":"e9","kind":"PauseEvent"}],"next_page_id":null}`))
			})
		}},
		{"a next page that cannot be asked for", StallNextPageUnreadable, func(t *testing.T) *Harness {
			return serve(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"items":[{"id":"e1","kind":"PauseEvent"}],"next_page_id":42}`))
			})
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fb := &fakeBroker{}
			r := newRig(t, c.harness(t), fb)
			ctx, stop := r.run(t)
			eventually(ctx, t, "the stall is counted three times", func() bool {
				return r.counter(t, metricStalls, c.reason) >= 3
			})
			eventually(ctx, t, "the room is told", func() bool {
				return slices.ContainsFunc(fb.stored(wire.StreamStatus), func(it wire.Item) bool { return payloadHas(it, c.reason) })
			})
			stop()
			if got := seqs(t, fb.stored(wire.StreamEvents)); !slices.Equal(got, []int64{SeqFor(1, 0)}) {
				t.Fatalf("mirrored %v: only the event before the stall may land", got)
			}
			told := 0
			for _, it := range fb.stored(wire.StreamStatus) {
				if payloadHas(it, c.reason) {
					told++
				}
			}
			if told != 1 {
				t.Fatalf("the room was told %d times, want once per stall", told)
			}
			logs := r.logs.String()
			if !strings.Contains(logs, `"level":"ERROR"`) || !strings.Contains(logs, c.reason) {
				t.Fatalf("no Error log names %s:\n%s", c.reason, logs)
			}
		})
	}
}

func TestShutdownFlushesWhatIsBuffered(t *testing.T) {
	f := &fakeAgentServer{pageSize: 100, status: "running"}
	for range 3 {
		f.add(chatEvent("hi"))
	}
	fb := &fakeBroker{hold: true}
	r := newRig(t, NewHarness(f.start(t, conv).URL, conv), fb)
	ctx, stop := r.run(t)
	eventually(ctx, t, "the broker refused a batch", func() bool { return slices.Contains(fb.callLog(), "events Service Unavailable") })
	fb.set(func(b *fakeBroker) { b.hold = false })
	stop() // SIGTERM: Run drains what is buffered before it returns
	if got := seqs(t, fb.stored(wire.StreamEvents)); len(got) != 3 {
		t.Fatalf("flushed %v on shutdown, want three events", got)
	}
}

func TestHealthOnlyFailsAfterTheHarnessWentAway(t *testing.T) {
	b := &Bridge{}
	now := time.Now()
	if !b.Healthy(now) {
		t.Fatal("before the harness answers once, a native sidecar must stay healthy (ruling P6)")
	}
	b.sawHarness(now.Add(-2 * time.Minute))
	if b.Healthy(now) {
		t.Fatal("unreachable for 2 min after being seen: unhealthy")
	}
	b.sawHarness(now)
	if !b.Healthy(now) {
		t.Fatal("seen now: healthy")
	}
}

func TestStreamFramesReachTheHooks(t *testing.T) {
	f := &fakeAgentServer{pageSize: 100, status: "running"}
	fb := &fakeBroker{sse: []string{": ping\n\n", "event: deliver\ndata: {\"ref\":7,\"text\":\"go\"}\n\n",
		"event: interrupt\ndata: {\"ref\":8}\n\n", "event: decision\ndata: {\"approvalId\":\"a1\",\"allow\":true}\n\n"}}
	r := newRig(t, NewHarness(f.start(t, conv).URL, conv), fb)
	got := make(chan string, 3)
	r.b.OnDeliver = func(_ context.Context, d wire.Deliver) { got <- fmt.Sprint("deliver ", d.Ref, " ", d.Text) }
	r.b.OnInterrupt = func(_ context.Context, i wire.Interrupt) { got <- fmt.Sprint("interrupt ", i.Ref) }
	r.b.OnDecision = func(_ context.Context, d wire.Decision) { got <- fmt.Sprint("decision ", d.ApprovalID, " ", d.Allow) }
	ctx, stop := r.run(t)
	var seen []string
	for len(seen) < 3 {
		select {
		case s := <-got:
			seen = append(seen, s)
		case <-ctx.Done():
			t.Fatalf("frames seen: %v", seen)
		}
	}
	stop()
	if !slices.Equal(seen, []string{"deliver 7 go", "interrupt 8", "decision a1 true"}) {
		t.Fatalf("frames seen: %v", seen)
	}
}
