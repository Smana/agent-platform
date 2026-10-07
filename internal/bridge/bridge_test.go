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
		case it.Seq == 12 && (!payloadHas(it, `"harnessKind":"refused"`) || !payloadHas(it, `"reason":"bad_item"`) ||
			!payloadHas(it, `"detail":"message"`) || payloadHas(it, "poison")):
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

// SIGTERM (review I1): the drain ignores the loop's backoff, waits at most
// drainWait after a refusal of its own, and sends the buffer before it polls a
// harness that may hang. The loop's own sendAt is 30 s out in every case, so
// only the drain can deliver.
func TestTheSIGTERMDrainAlwaysSends(t *testing.T) {
	busy := reply{code: http.StatusServiceUnavailable, reason: wire.ReasonLogUnavailable, retryAfter: "30"}
	cases := []struct {
		name    string
		replies []reply
		hang    bool
		within  time.Duration
	}{
		{"the drain does not wait out the loop's backoff", []reply{busy}, false, time.Second},
		{"a refusal inside the drain waits at most drainWait", []reply{busy, busy}, false, drainWait + time.Second},
		{"the buffer goes before a hung harness is polled", []reply{busy}, true, 4 * time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeAgentServer{pageSize: 100, status: "running"}
			for range 3 {
				f.add(chatEvent("hi"))
			}
			fb := &fakeBroker{events: c.replies}
			r := newRig(t, NewHarness(f.start(t, conv).URL, conv), fb)
			r.b.MaxBackoff, r.b.FlushGrace = time.Hour, 3500*time.Millisecond
			ctx, _ := r.run(t)
			eventually(ctx, t, "the broker refused a batch", func() bool {
				return slices.Contains(fb.callLog(), "events Service Unavailable")
			})
			time.Sleep(30 * time.Millisecond) // the loop honours the 30 s Retry-After
			if n := len(fb.stored(wire.StreamEvents)); n != 0 {
				t.Fatalf("the loop sent %d events inside its backoff: the case tests nothing", n)
			}
			if c.hang {
				f.mu.Lock()
				f.hang = true
				f.mu.Unlock()
			}
			sigterm := time.Now()
			r.cancel()
			dctx, dcancel := context.WithTimeout(t.Context(), 6*time.Second)
			defer dcancel()
			eventually(dctx, t, "the drain delivers", func() bool { return len(fb.stored(wire.StreamEvents)) == 3 })
			if took := time.Since(sigterm); took > c.within {
				t.Fatalf("delivered %v after SIGTERM, want within %v", took, c.within)
			}
			<-r.done
			seqs(t, fb.stored(wire.StreamEvents))
		})
	}
}

// F11: agent-server's cold start under gVisor outlasted MaxBackoff, so a short
// conversation ran and ended between two polls. Until the harness first
// answers, a failed read waits at most firstContactPolls intervals. The status
// never changes here, so only that cap can catch the conversation.
func TestTheLogIsPolledOftenUntilTheHarnessFirstAnswers(t *testing.T) {
	f := &fakeAgentServer{pageSize: 100, down: true}
	for i := range 3 {
		f.add(chatEvent(fmt.Sprint("hi ", i)))
	}
	fb := &fakeBroker{}
	r := newRig(t, NewHarness(f.start(t, conv).URL, conv), fb)
	r.b.Interval, r.b.MaxBackoff = 20*time.Millisecond, time.Hour
	ctx, stop := r.run(t)
	// After nine refusals from 1 ms, the next wait is 256 ms uncapped and 200 ms
	// under the answeredPolls cap: longer than the conversation.
	eventually(ctx, t, "nine reads were refused", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.cut >= 9
	})
	f.setDown(false)
	time.Sleep(120 * time.Millisecond)
	f.setDown(true) // the harness exits before SIGTERM
	stop()
	if got := seqs(t, fb.stored(wire.StreamEvents)); !slices.Equal(got, []int64{SeqFor(1, 0), SeqFor(2, 0), SeqFor(3, 0)}) {
		t.Fatalf("mirrored %v of a conversation that ran for 120 ms", got)
	}
}

// Once the harness has answered, a failing read backs off past the
// first-contact cap, up to answeredPolls intervals.
func TestAnAnsweredLogBacksOffFurther(t *testing.T) {
	f := &fakeAgentServer{pageSize: 100}
	f.add(chatEvent("one"))
	served := false
	f.failSearch = func() bool {
		defer func() { served = true }()
		return served
	}
	fb := &fakeBroker{}
	r := newRig(t, NewHarness(f.start(t, conv).URL, conv), fb)
	r.b.MaxBackoff = time.Hour
	ctx, stop := r.run(t)
	eventually(ctx, t, "the log answered", func() bool { return len(fb.stored(wire.StreamEvents)) == 1 })
	time.Sleep(300 * time.Millisecond) // the backoff reaches its cap
	before := f.searched()
	time.Sleep(300 * time.Millisecond)
	n := f.searched() - before
	stop()
	// 300 ms is 6 polls at answeredPolls × 5 ms, 30 at firstContactPolls × 5 ms.
	if n < 1 || n > 12 {
		t.Fatalf("%d reads of a failing log in 300 ms, want about 6", n)
	}
}

// F11: agent-run stops agent-server soon after the conversation ends. A status
// change reads the log to its end at once, whatever the poll's backoff.
func TestAStatusChangeReadsTheLogToItsEnd(t *testing.T) {
	f := &fakeAgentServer{pageSize: 2, status: "running"}
	f.add(chatEvent("first"))
	served, failed := false, 0
	f.failSearch = func() bool { // under f.mu
		if !served {
			served = true
			return false
		}
		if f.status == "finished" {
			return false
		}
		if failed++; failed == 10 {
			// The conversation ends right after a failed read, whose retry is a
			// whole backoff (10 × 50 ms) away, past the harness's exit.
			for i := range 4 {
				f.events = append(f.events, chatEvent(fmt.Sprint("last ", i)))
				f.events[len(f.events)-1]["id"] = fmt.Sprint("e", len(f.events))
			}
			f.status = "finished"
			time.AfterFunc(250*time.Millisecond, func() { f.setDown(true) })
		}
		return true
	}
	fb := &fakeBroker{}
	r := newRig(t, NewHarness(f.start(t, conv).URL, conv), fb)
	r.b.Interval, r.b.MaxBackoff = 50*time.Millisecond, time.Hour
	ctx, stop := r.run(t)
	eventually(ctx, t, "the harness exited", func() bool {
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.down
	})
	stop()
	want := []int64{SeqFor(1, 0), SeqFor(2, 0), SeqFor(3, 0), SeqFor(4, 0), SeqFor(5, 0)}
	if got := seqs(t, fb.stored(wire.StreamEvents)); !slices.Equal(got, want) {
		t.Fatalf("mirrored %v, want %v", got, want)
	}
}

// F11: the SIGTERM drain reads what the log has left to its end, not one poll's
// worth, and positions a cursor the loop never could.
func TestTheSIGTERMDrainReadsTheLogToItsEnd(t *testing.T) {
	all := []int64{SeqFor(1, 0), SeqFor(2, 0), SeqFor(3, 0), SeqFor(4, 0), SeqFor(5, 0), SeqFor(6, 0), SeqFor(7, 0)}
	cases := []struct {
		name   string
		resume int64
		want   []int64
	}{
		{"more than one poll's worth", 0, all},
		{"a bridge that never positioned", SeqFor(2, 0), all[1:]},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeAgentServer{pageSize: 2, status: "running"}
			for i := range 7 {
				f.add(chatEvent(fmt.Sprint("hi ", i)))
			}
			failing := true
			f.failSearch = func() bool { return failing }
			fb := &fakeBroker{resume: wire.Resume{RoomID: "3kq7x2ma", AfterHarnessSeq: c.resume}}
			r := newRig(t, NewHarness(f.start(t, conv).URL, conv), fb)
			r.b.MaxBackoff = time.Hour
			ctx, _ := r.run(t)
			eventually(ctx, t, "a read failed", func() bool { return f.searched() > 1 })
			f.mu.Lock()
			failing = false
			f.mu.Unlock()
			r.cancel()
			<-r.done
			if got := seqs(t, fb.stored(wire.StreamEvents)); !slices.Equal(got, c.want) {
				t.Fatalf("mirrored %v, want %v", got, c.want)
			}
		})
	}
}

// Review I3: a full buffer stops reading the harness; once the broker drains
// it, reading resumes and every event lands once.
func TestBackPressureStopsPollingUntilTheBufferDrains(t *testing.T) {
	f := &fakeAgentServer{pageSize: 2, status: "running"}
	for range 10 {
		f.add(chatEvent(strings.Repeat("x", 1<<10)))
	}
	fb := &fakeBroker{hold: true}
	r := newRig(t, NewHarness(f.start(t, conv).URL, conv), fb)
	r.b.MaxBuffer = 2 << 10 // one page of two events fills it
	ctx, stop := r.run(t)
	eventually(ctx, t, "the first page fills the buffer", func() bool {
		return f.searched() > 0 && slices.Contains(fb.callLog(), "events Service Unavailable")
	})
	time.Sleep(20 * time.Millisecond) // the poll in flight, if any, lands
	full := f.searched()
	time.Sleep(50 * time.Millisecond) // ten polls
	if n := f.searched(); n != full {
		t.Fatalf("the harness was read %d more times with the buffer full", n-full)
	}
	fb.set(func(b *fakeBroker) { b.hold = false })
	eventually(ctx, t, "every event lands", func() bool { return len(fb.stored(wire.StreamEvents)) == 10 })
	stop()
	if got := seqs(t, fb.stored(wire.StreamEvents)); got[9] != SeqFor(10, 0) {
		t.Fatalf("seqs = %v", got)
	}
}

// Review I3: a 410 seals the room. The bridge stops pushing and stays healthy,
// since a restart cannot help, and Run still returns on SIGTERM.
func TestASealedRoomStopsTheBridge(t *testing.T) {
	sealed := reply{code: http.StatusGone, reason: wire.ReasonSealed}
	cases := []struct {
		name   string
		broker *fakeBroker
	}{
		{"sealed at hello", &fakeBroker{hellos: []reply{sealed}}},
		{"sealed on a batch", &fakeBroker{events: []reply{sealed}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &fakeAgentServer{pageSize: 100, status: "running"}
			f.add(chatEvent("hi"))
			fb := c.broker
			r := newRig(t, NewHarness(f.start(t, conv).URL, conv), fb)
			ctx, _ := r.run(t)
			eventually(ctx, t, "the broker sealed the room", func() bool {
				return slices.ContainsFunc(fb.callLog(), func(c string) bool { return strings.HasSuffix(c, " Gone") })
			})
			calls := len(fb.callLog())
			time.Sleep(50 * time.Millisecond) // ten polls
			if got := fb.callLog(); len(got) != calls {
				t.Fatalf("the bridge kept calling a sealed room: %v", got)
			}
			if !r.b.Healthy(time.Now().Add(2 * unreachableFail)) {
				t.Fatal("a sealed room made the bridge unhealthy: the kubelet would restart it for nothing")
			}
			r.cancel()
			select {
			case <-r.done:
			case <-time.After(time.Second):
				t.Fatal("Run did not return on SIGTERM")
			}
			if n := len(fb.stored(wire.StreamEvents)); n != 0 {
				t.Fatalf("%d events reached a sealed room", n)
			}
		})
	}
}

// Review I2: the worst-case heap of the bridge fits the soft memory limit
// room-bridge runs under: the buffer before a poll, what one Next retains and
// reads, and the items that poll maps.
func TestTheWorstCaseHeapFitsTheMemoryLimit(t *testing.T) {
	// Events are read only below MaxBuffer; status items stop at statusCap.
	worst := max(DefaultMaxBuffer+nextPeakBytes(DefaultMaxPages)+pageItemBytes(DefaultMaxPages),
		statusCap(DefaultMaxBuffer)+statusItemsPerPoll*maxItemBytes)
	if worst > MemoryLimit {
		t.Fatalf("worst case %d MiB > MemoryLimit %d MiB", worst>>20, MemoryLimit>>20)
	}
	if MemoryLimit > 48<<20 {
		t.Fatalf("MemoryLimit %d MiB leaves the 64 Mi sidecar too little headroom", MemoryLimit>>20)
	}
}

// Review N1: status items pass a full buffer, but only up to statusCap, so a
// lease lost for good cannot grow the buffer without bound.
func TestStatusItemsAreBounded(t *testing.T) {
	f := &fakeAgentServer{pageSize: 100, status: "running", flap: true}
	fb := &fakeBroker{hold: true}
	r := newRig(t, NewHarness(f.start(t, conv).URL, conv), fb)
	r.b.MaxBuffer, r.b.FlushGrace = 1<<10, 50*time.Millisecond
	ctx, stop := r.run(t)
	eventually(ctx, t, "a batch was refused", func() bool {
		return slices.Contains(fb.callLog(), "events Service Unavailable")
	})
	time.Sleep(300 * time.Millisecond) // about sixty status changes, a few KiB of items
	stop()
	// One poll past the cap adds two items, each well under 1 KiB for these short statuses.
	if limit := statusCap(r.b.MaxBuffer) + 1<<10; r.b.bufBytes > limit {
		t.Fatalf("the buffer holds %d bytes, over its %d-byte cap", r.b.bufBytes, limit)
	}
	if r.b.bufBytes <= r.b.MaxBuffer {
		t.Fatalf("the buffer holds %d bytes: status items stopped at MaxBuffer, the case tests nothing", r.b.bufBytes)
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
	r.b.OnDeliver = func(_ context.Context, d wire.Deliver) error {
		got <- fmt.Sprint("deliver ", d.Ref, " ", d.Text)
		return nil
	}
	r.b.OnInterrupt = func(_ context.Context, i wire.Interrupt) error { got <- fmt.Sprint("interrupt ", i.Ref); return nil }
	r.b.OnDecision = func(_ context.Context, d wire.Decision) error {
		got <- fmt.Sprint("decision ", d.ApprovalID, " ", d.Allow)
		return nil
	}
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

// F11, the harness half (disruption design §2): a final read answers only once the log is read
// to its end and mirrored, whatever the loop's own pace.
func TestAFinalReadMirrorsTheLogBeforeItAnswers(t *testing.T) {
	f := &fakeAgentServer{pageSize: 2, status: "running"}
	for i := range 3 {
		f.add(chatEvent(fmt.Sprint("hi ", i)))
	}
	fb := &fakeBroker{}
	r := newRig(t, NewHarness(f.start(t, conv).URL, conv), fb)
	r.b.Interval = time.Hour // one step, then the loop idles: only a final read moves the log
	ctx, _ := r.run(t)
	eventually(ctx, t, "the first step mirrors the log", func() bool { return len(fb.stored(wire.StreamEvents)) == 3 })
	for i := 3; i < 7; i++ {
		f.add(chatEvent(fmt.Sprint("hi ", i)))
	}
	res, err := r.b.FinalRead(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := []int64{SeqFor(1, 0), SeqFor(2, 0), SeqFor(3, 0), SeqFor(4, 0), SeqFor(5, 0), SeqFor(6, 0), SeqFor(7, 0)}
	if got := seqs(t, fb.stored(wire.StreamEvents)); !slices.Equal(got, want) || res != (FinalReadResult{Events: 7}) {
		t.Fatalf("answered %+v with %v mirrored, want every event mirrored first: %v", res, got, want)
	}
}

// A room sealed mid-run has nothing left to mirror: a final read answers so at once, reading the
// log no further and calling the broker no more.
func TestAFinalReadOnASealedRoomAnswersSealed(t *testing.T) {
	f := &fakeAgentServer{pageSize: 100, status: "running"}
	f.add(chatEvent("hi"))
	fb := &fakeBroker{events: []reply{{code: http.StatusGone, reason: wire.ReasonSealed}}}
	r := newRig(t, NewHarness(f.start(t, conv).URL, conv), fb)
	ctx, _ := r.run(t)
	eventually(ctx, t, "the broker sealed the room", func() bool {
		return slices.ContainsFunc(fb.callLog(), func(c string) bool { return strings.HasSuffix(c, " Gone") })
	})
	calls := len(fb.callLog())
	f.add(chatEvent("after the seal"))
	res, err := r.b.FinalRead(ctx)
	if err != nil || res != (FinalReadResult{Events: 1, Unmirrored: res.Unmirrored, Sealed: true}) {
		t.Fatalf("got %+v, %v: want a sealed answer over the one event read before the seal", res, err)
	}
	if got := fb.callLog(); len(got) != calls {
		t.Fatalf("the final read called a sealed room: %v", got[calls:])
	}
}

// A final read is bounded by its caller: a broker that keeps refusing still gets the caller an
// answer naming what is unmirrored, before the caller's deadline.
func TestAFinalReadAnswersBeforeItsDeadline(t *testing.T) {
	f := &fakeAgentServer{pageSize: 2, status: "running"}
	f.add(chatEvent("hi"))
	fb := &fakeBroker{}
	r := newRig(t, NewHarness(f.start(t, conv).URL, conv), fb)
	r.b.Interval = time.Hour
	ctx, _ := r.run(t)
	eventually(ctx, t, "the first step mirrors the log", func() bool { return len(fb.stored(wire.StreamEvents)) == 1 })
	fb.set(func(b *fakeBroker) { b.hold = true })
	f.add(chatEvent("unmirrored"))
	call, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	start := time.Now()
	res, err := r.b.FinalRead(call)
	if took := time.Since(start); err != nil || res.Unmirrored == 0 || res.Events != 2 || took >= time.Second {
		t.Fatalf("got %+v, %v after %v: want an answer naming the unmirrored items inside the deadline", res, err, took)
	}
}
