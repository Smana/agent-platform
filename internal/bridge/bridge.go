// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/wire"
)

const (
	// defaultFlushGrace bounds the drain after SIGTERM, inside the pod's 30 s
	// grace. The kubelet signals a native sidecar only once the harness has
	// exited, so a pod spec whose harness uses much of the grace sets FlushGrace
	// lower (FLUSH_GRACE).
	defaultFlushGrace = 25 * time.Second
	// drainWait caps each wait inside the drain, whatever the backoff or a
	// Retry-After says: past SIGTERM, one more attempt beats waiting politely.
	drainWait = 2 * time.Second
	// unreachableFail is how long the harness may be silent, once seen, before
	// /healthz fails (ruling P6).
	unreachableFail = 60 * time.Second
	// batchItems is the most items a batch carries; the broker takes 500.
	batchItems = 100
	// maxBatchBytes is the broker's body cap (bridgeapi: 2 MiB), measured on the
	// encoded batch, JSON escaping included. A broker with a smaller cap
	// answers 413, and the batch is halved.
	maxBatchBytes = 2 << 20
	// maxRetryWait caps a Retry-After, so a broker's answer cannot park the bridge.
	maxRetryWait = 30 * time.Second
	// heartbeatEvery is the longest the bridge goes without renewing its room's
	// lease: an empty batch when nothing else was accepted. A quiet run (a long LLM
	// call, a pending confirmation) pushes no items (review I2), and the heartbeat
	// is fenced like an append, so a displaced bridge learns it within 30 s. Time
	// alone never frees a lease (ruling SBB).
	heartbeatEvery = 30 * time.Second
	// busyPatience is how long the first hello may hear room_busy before the run
	// is refused for good (F15): how long a second run waits for the holder's run
	// to end. The broker frees a lease only once its run ends (ruling SBB), and the
	// factory starts the next run only after that, so this covers the broker's
	// watch catching up, never a holder at work.
	busyPatience = 3 * time.Minute

	defaultInterval   = time.Second
	defaultMinBackoff = 250 * time.Millisecond
	defaultMaxBackoff = 30 * time.Second
	// The harness is on loopback, so MaxBackoff, sized for the broker, is too
	// slow for it (F11): agent-server's cold start under gVisor (~85 s) grew the
	// wait past a whole conversation, which ended unread. Until the harness first
	// answers, a failed read of its log waits at most firstContactPolls
	// intervals; after, at most answeredPolls.
	firstContactPolls = 2
	answeredPolls     = 10
	// MemoryLimit is the soft heap limit room-bridge sets (GOMEMLIMIT) inside
	// the sidecar's 64 Mi limit, leaving the rest to the runtime and stacks.
	MemoryLimit = 48 << 20
	// DefaultMaxBuffer is the MaxBuffer a zero value means. The heap's worst
	// case is the buffer before a poll, plus what that poll's Next retains and
	// reads, plus the items it maps; TestTheWorstCaseHeapFitsTheMemoryLimit
	// pins the sum under MemoryLimit.
	DefaultMaxBuffer = 3 << 20
	// maxItemBytes is one encoded item at most: a payload within MaxPayload
	// and the item's stream, seq and type around it.
	maxItemBytes = envelope.MaxPayload + 256

	metricStalls  = "rooms_bridge_harness_stalls_total"
	metricStubbed = "rooms_bridge_items_stubbed_total"
)

// The reasons the harness's log stops moving: each is an adapter error that
// holds the cursor where it is (Task 1.10). They label rooms_bridge_harness_stalls_total
// and are the code of the harness_error the room is told.
const (
	StallEventTooLarge      = "event_too_large"
	StallCursorLost         = "cursor_lost"
	StallNextPageUnreadable = "next_page_unreadable"
)

// The reasons an item is replaced by a stub in its slot, labelling
// rooms_bridge_items_stubbed_total.
const (
	StubOversize = "oversize" // alone over the broker's body cap
	StubRefused  = "refused"  // refused by the broker for what it is (400)
)

// pageItemBytes is the most one poll adds to the buffer: maxPages pages of
// pageLimit events, each mapped to at most two items.
func pageItemBytes(maxPages int) int { return max(maxPages, 1) * pageLimit * 2 * maxItemBytes }

// statusItemsPerPoll is the most items one status poll adds.
const statusItemsPerPoll = 2

// statusCap is where status items stop too (review N1). They pass MaxBuffer,
// since a transition missed is a turn lost, but a lease lost for good must not
// grow the buffer for as long as the pod lives. Events are read only below
// MaxBuffer, so a poll never runs with the buffer this full.
func statusCap(maxBuffer int) int { return 2 * maxBuffer }

// nextPeakBytes is what one Next holds while it runs: maxPages retained
// bodies and one more being read (Task 1.10).
func nextPeakBytes(maxPages int) int { return (max(maxPages, 1) + 1) * maxResponseBytes }

func stallReason(err error) string {
	switch {
	case errors.Is(err, ErrEventTooLarge):
		return StallEventTooLarge
	case errors.Is(err, ErrCursorLost):
		return StallCursorLost
	case errors.Is(err, ErrNextPageUnreadable):
		return StallNextPageUnreadable
	}
	return ""
}

// StallNotice is the status item that tells the room the harness's log stopped
// moving for reason. Nothing after that point is mirrored until it moves again.
func StallNotice(reason string) Mapped {
	return state("harness_error", map[string]string{"code": reason,
		"detail": "the bridge cannot read past this point of the harness log; it keeps trying"})
}

// Stub stands in for an item of type t and n payload bytes that the broker
// cannot take as sent (why is StubOversize or StubRefused; reason is the
// broker's wire reason, if any). Its slot is kept so the cursor moves on and
// nothing after it is lost (ruling P20, review I6). Only turn, tool_call and
// tool_result accept the store's oversize stub; any other item becomes a
// harness_event naming why, the item's type (detail), its size and the reason.
func Stub(t envelope.Type, n int, why, reason string) Mapped {
	if why == StubOversize && (t == envelope.Turn || t == envelope.ToolCall || t == envelope.ToolResult) {
		return Mapped{t, envelope.Oversize(t, n)}
	}
	fields := map[string]any{"harnessKind": why, "detail": cut(string(t), 64), "bytes": n}
	if reason != "" {
		fields["reason"] = cut(reason, 64)
	}
	return Mapped{envelope.StateChanged, envelope.StatePayload("harness_event", fields)}
}

// pending is a buffered item. Only its encoding holds the payload, measured
// once and sent as is, so bufBytes counts what the buffer holds (review I2).
type pending struct {
	stream wire.Stream
	seq    int64
	typ    envelope.Type
	n      int // the payload's bytes before encoding
	enc    json.RawMessage
	stub   string // why the item is a stub, "" when it is as mapped
}

func encode(it wire.Item) pending {
	p := pending{stream: it.Stream, seq: it.Seq, typ: it.Type, n: len(it.Payload)}
	enc, err := json.Marshal(it)
	if err != nil {
		// Only a payload that is not JSON fails, which Map never produces.
		return encodeStub(p, StubRefused, "")
	}
	p.enc = enc
	return p
}

func encodeStub(p pending, why, reason string) pending {
	m := Stub(p.typ, p.n, why, reason)
	enc, _ := json.Marshal(wire.Item{Stream: p.stream, Seq: p.seq, Type: m.Type, Payload: m.Payload}) // Stub's payloads are always JSON
	return pending{stream: p.stream, seq: p.seq, typ: m.Type, n: len(m.Payload), enc: enc, stub: why}
}

// nextBatch is how many items from the front of buf the next batch carries:
// at most maxItems, and as many as encode within maxBytes. It is at least one
// while buf is not empty: a lone item over the cap is the caller's to stub.
func nextBatch(buf []pending, maxBytes, maxItems int) int {
	size, n := batchOverhead(0), 0
	for n < len(buf) && n < maxItems {
		add := len(buf[n].enc)
		if n > 0 {
			add++ // the comma
		}
		if n > 0 && size+add > maxBytes {
			break
		}
		size += add
		n++
	}
	return n
}

func batchBody(p []pending) []byte { return joinBatch(encoded(p)) }

func encoded(p []pending) []json.RawMessage {
	out := make([]json.RawMessage, len(p))
	for i := range p {
		out[i] = p[i].enc
	}
	return out
}

// retryDelay reads a Retry-After, in seconds or as an HTTP date, capped at
// maxRetryWait. Absent or unreadable, it is fallback.
func retryDelay(header string, now time.Time, fallback time.Duration) time.Duration {
	header = strings.TrimSpace(header)
	if s, err := strconv.Atoi(header); err == nil {
		switch {
		case s < 0:
			return fallback
		case s >= int(maxRetryWait/time.Second):
			return maxRetryWait
		}
		return time.Duration(s) * time.Second
	}
	if t, err := http.ParseTime(header); err == nil {
		return min(max(t.Sub(now), 0), maxRetryWait)
	}
	return fallback
}

// backoff doubles from lo to hi.
type backoff struct{ cur time.Duration }

func (b *backoff) next(lo, hi time.Duration) time.Duration {
	if b.cur < lo {
		b.cur = lo
	} else {
		b.cur = min(2*b.cur, hi)
	}
	return b.cur
}

func (b *backoff) reset() { b.cur = 0 }

// pause waits d, or until ctx ends.
func pause(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(max(d, 0))
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Bridge mirrors one run's harness into its room: it polls the harness, maps
// its events, and pushes them to the broker in batches, never dropping one. A
// broker outage never stops the sandbox (§3): the harness keeps its own store,
// and the bridge resumes from the log's cursor.
type Bridge struct {
	Harness *Harness
	Broker  *Broker
	RunID   string
	// Interval is the poll period; zero means 1 s.
	Interval time.Duration
	// MaxBuffer caps the encoded bytes waiting for the broker; past it the
	// harness's events are not read (back-pressure), so the buffer holds at
	// most MaxBuffer plus one poll's items. Status changes are still recorded
	// up to twice MaxBuffer. Zero means DefaultMaxBuffer.
	MaxBuffer int
	// FlushGrace bounds the drain after SIGTERM; zero means 25 s.
	FlushGrace time.Duration
	// MinBackoff and MaxBackoff bound the retry of a failed call; zero means
	// 250 ms and 30 s.
	MinBackoff, MaxBackoff time.Duration
	Logger                 *slog.Logger
	// Meter makes the rooms_bridge_* instruments; nil means a no-op meter.
	// room-bridge passes none: nothing scrapes a sandbox (C4), so the broker
	// counts the same signals from the events it appends (Ruling AP).
	Meter metric.Meter
	// Now is the clock; nil means time.Now.
	Now func() time.Time

	// The stream's hooks. An error ends the stream, which is re-dialled and
	// replays from the log's last acknowledgement.
	OnDeliver   func(ctx context.Context, d wire.Deliver) error   // phase 4
	OnInterrupt func(ctx context.Context, i wire.Interrupt) error // phase 4
	OnDecision  func(ctx context.Context, d wire.Decision) error  // phase 5
	OnResume    func(ctx context.Context, r wire.Resume)          // phase 5

	// The confirmation loop's hooks (phase 5), called on the loop except OnReady.
	//
	// OnReady runs from the start, before the broker's hello and whatever it
	// answers, until it succeeds (review C2). OnRaw sees every harness event read, those a restart skips
	// included, before it is mapped. OnStatus gets a status only after an
	// events poll that started after that status was read and reached the
	// log's end, so every action the harness wrote before it was passed to
	// OnRaw. Classify sets each tool_call's class.
	OnReady  func(ctx context.Context) error
	OnRaw    func(e RawEvent)
	OnStatus func(ctx context.Context, status string)
	Classify func(tool string, action json.RawMessage, risk string) string

	// inbox holds what Push hands over from other goroutines, until the loop,
	// the only one to touch buf and status, takes it.
	inMu  sync.Mutex
	inbox []wire.Item

	buf        []pending
	bufBytes   int
	resumeAt   int64 // the log's AfterHarnessSeq at the first hello
	positioned bool  // the cursor is past what the log holds
	cursor     Cursor
	status     StatusTracker
	stall      string // the stall reason the room was last told, "" once the log moves
	pollAt     time.Time
	sendAt     time.Time
	pollRetry  backoff
	sendRetry  backoff
	leaseLost  bool
	renewedAt  time.Time // the broker last renewed the lease: a hello or an accepted batch
	lastStatus string    // the status the previous step read, "" if it failed

	sealed    atomic.Bool
	lastSeen  atomic.Int64
	admission atomic.Pointer[Admission]

	stalls  metric.Int64Counter
	stubbed metric.Int64Counter
}

func (b *Bridge) log() *slog.Logger {
	if b.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return b.Logger
}

func (b *Bridge) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

func (b *Bridge) backoffs() (lo, hi time.Duration) {
	lo, hi = b.MinBackoff, b.MaxBackoff
	if lo <= 0 {
		lo = defaultMinBackoff
	}
	if hi <= 0 {
		hi = defaultMaxBackoff
	}
	return lo, max(lo, hi)
}

// pollBackoffs bound the retry of a failed read of the harness log.
func (b *Bridge) pollBackoffs() (lo, hi time.Duration) {
	lo, hi = b.backoffs()
	polls := time.Duration(firstContactPolls)
	if b.lastSeen.Load() != 0 {
		polls = answeredPolls
	}
	return lo, max(lo, min(hi, polls*b.Interval))
}

func (b *Bridge) sawHarness(t time.Time) { b.lastSeen.Store(t.UnixNano()) }

// Admission is what the bridge's first hellos decided (F15). The zero value is
// pending. room-bridge gate holds the harness until it is decided, and fails
// the pod on a refusal, so no run executes unless its bridge holds the room.
type Admission struct {
	Admitted bool   // the broker handed this run the room's bridge lease
	Refused  string // why it never will: wire.ReasonRoomBusy or wire.ReasonSealed
}

// Admission is the decision so far; safe from any goroutine.
func (b *Bridge) Admission() Admission {
	if a := b.admission.Load(); a != nil {
		return *a
	}
	return Admission{}
}

// Healthy backs /healthz (ruling P6): a native sidecar must not fail before the
// harness starts, since its startup probe gates the harness container. A
// sealed room has nothing left to mirror, which is not a failure.
func (b *Bridge) Healthy(now time.Time) bool {
	seen := b.lastSeen.Load()
	return seen == 0 || b.sealed.Load() || now.Sub(time.Unix(0, seen)) <= unreachableFail
}

func (b *Bridge) init() error {
	m := b.Meter
	if m == nil {
		m = noop.NewMeterProvider().Meter("")
	}
	var err error
	if b.stalls, err = m.Int64Counter(metricStalls,
		metric.WithDescription("Polls the harness log could not move past, by reason; the cursor holds.")); err != nil {
		return fmt.Errorf("bridge: %s: %w", metricStalls, err)
	}
	if b.stubbed, err = m.Int64Counter(metricStubbed,
		metric.WithDescription("Items the broker could not take as sent, kept as a stub in their slot, by reason.")); err != nil {
		return fmt.Errorf("bridge: %s: %w", metricStubbed, err)
	}
	if b.Interval <= 0 {
		b.Interval = defaultInterval
	}
	if b.MaxBuffer <= 0 {
		b.MaxBuffer = DefaultMaxBuffer
	}
	if b.FlushGrace <= 0 {
		b.FlushGrace = defaultFlushGrace
	}
	return nil
}

// Run mirrors the conversation into the room until ctx ends, then flushes what
// is buffered within FlushGrace. It returns an error only if it cannot start.
func (b *Bridge) Run(ctx context.Context) error {
	if err := b.init(); err != nil {
		return err
	}
	var wg sync.WaitGroup
	defer wg.Wait()
	// Confirmation mode never waits for the broker: an outage or a lease held
	// by another run must not leave the harness on NeverConfirm (review C2).
	wg.Go(func() { b.confirmMode(ctx) })
	resume, ok := b.connect(ctx)
	if !ok {
		// Sealed or refused: stay up, idle, until the pod ends. room-bridge gate
		// fails the pod on a refusal before the harness starts.
		<-ctx.Done()
		return nil
	}
	b.resumeAt = resume.AfterHarnessSeq
	b.status.Resume(resume.AfterStatusSeq)
	if b.OnResume != nil {
		b.OnResume(ctx, resume)
	}
	wg.Go(func() { b.consume(ctx) })
	for {
		b.step(ctx)
		if pause(ctx, b.Interval) != nil {
			b.shutdown()
			return nil
		}
	}
}

// confirmMode calls OnReady until it succeeds or ctx ends, talking only to the
// harness. A conversation agent-run has not created yet answers 404. It
// retries at the minimum backoff (250 ms), never doubling: every retry while
// the conversation exists is time its actions run unconfirmed (ruling P5).
func (b *Bridge) confirmMode(ctx context.Context) {
	if b.OnReady == nil {
		return
	}
	every, _ := b.backoffs()
	for {
		err := b.OnReady(ctx)
		if err == nil {
			return
		}
		if se, ok := errors.AsType[*StatusError](err); ok && se.Code == http.StatusNotFound {
			b.log().Debug("no conversation to set AlwaysConfirm on yet")
		} else if ctx.Err() == nil {
			b.log().Warn("the harness did not take AlwaysConfirm; retrying", "err", err)
		}
		if pause(ctx, every) != nil {
			return
		}
	}
}

// connect says hello until the broker hands over the lease, and records the
// Admission. It reports false if ctx ended first, the room is sealed, or
// another run kept the room for busyPatience.
func (b *Bridge) connect(ctx context.Context) (wire.Resume, bool) {
	var busySince time.Time
	for {
		r, rep, ok := b.hello(ctx)
		switch {
		case ok:
			b.admission.Store(&Admission{Admitted: true})
			return r, true
		case b.sealed.Load():
			b.admission.Store(&Admission{Refused: wire.ReasonSealed})
			return wire.Resume{}, false
		case rep.Code == http.StatusConflict:
			if busySince.IsZero() {
				busySince = b.now()
			}
			if b.now().Sub(busySince) >= busyPatience {
				b.log().Error("another run kept the room; this run is refused and its harness never starts",
					"reason", wire.ReasonRoomBusy, "waited", busyPatience)
				b.admission.Store(&Admission{Refused: wire.ReasonRoomBusy})
				return wire.Resume{}, false
			}
		}
		if pause(ctx, b.sendAt.Sub(b.now())) != nil {
			return wire.Resume{}, false
		}
	}
}

// hello makes one attempt at the room's bridge lease; on failure sendAt is when
// to try again.
func (b *Bridge) hello(ctx context.Context) (wire.Resume, Reply, bool) {
	r, rep, err := b.Broker.Hello(ctx)
	if err != nil && ctx.Err() != nil {
		return r, rep, false // cut short by SIGTERM: not the broker's failure
	}
	switch {
	case err == nil && rep.Code == http.StatusOK:
		b.sendRetry.reset()
		b.sendAt = time.Time{}
		b.renewedAt = b.now()
		return r, rep, true
	case err == nil && rep.Code == http.StatusGone:
		b.seal()
		return r, rep, false
	case err == nil && rep.Code == http.StatusConflict:
		b.log().Info("another run holds the room's bridge lease; saying hello again later", "reason", rep.Reason)
	default:
		b.log().Warn("hello failed", "code", rep.Code, "reason", rep.Reason, "err", err)
	}
	b.sendAt = b.now().Add(b.retryIn(rep))
	return r, rep, false
}

// retryIn is the wait before the next call to the broker: its Retry-After on a
// 429 or 503, else the next backoff.
func (b *Bridge) retryIn(rep Reply) time.Duration {
	fallback := b.sendRetry.next(b.backoffs())
	if rep.Code == http.StatusTooManyRequests || rep.Code == http.StatusServiceUnavailable {
		return retryDelay(rep.RetryAfter, b.now(), fallback)
	}
	return fallback
}

func (b *Bridge) seal() {
	b.sealed.Store(true)
	b.log().Warn("the room is sealed; the bridge stops mirroring", "unmirrored", len(b.buf))
	b.buf, b.bufBytes = nil, 0
}

// Push adds an item from any goroutine, such as the stream's acknowledgements.
// A status item with no seq gets the status stream's next one when the loop
// takes it, so the stream stays numbered in one place.
func (b *Bridge) Push(it wire.Item) {
	b.inMu.Lock()
	defer b.inMu.Unlock()
	b.inbox = append(b.inbox, it)
}

// takeInbox buffers what Push handed over.
func (b *Bridge) takeInbox(ctx context.Context) {
	b.inMu.Lock()
	in := b.inbox
	b.inbox = nil
	b.inMu.Unlock()
	for _, it := range in {
		if it.Stream == wire.StreamStatus && it.Seq == 0 {
			b.pushStatus(ctx, b.status.Emit(Mapped{it.Type, it.Payload}))
			continue
		}
		b.push(ctx, it)
	}
}

// step is one poll and one flush, each when its backoff allows.
func (b *Bridge) step(ctx context.Context) {
	if b.sealed.Load() {
		return
	}
	b.takeInbox(ctx)
	now := b.now()
	caughtUp := false
	if !now.Before(b.pollAt) {
		caughtUp = b.pollEvents(ctx)
	}
	// The status the previous step read: the events poll above started after
	// it, so it saw every action written before it (and any answer the hook
	// gave came before the status read below).
	if caughtUp && b.lastStatus != "" && b.OnStatus != nil {
		b.OnStatus(ctx, b.lastStatus)
	}
	b.lastStatus = b.pollStatus(ctx)
	if !now.Before(b.sendAt) {
		b.send(ctx)
	}
}

// pollEvents reads and maps the harness events after the cursor, unless the
// buffer is full: the harness keeps its own store meanwhile. It reports that
// the read reached the log's end.
func (b *Bridge) pollEvents(ctx context.Context) bool {
	if b.bufBytes >= b.MaxBuffer {
		return false
	}
	if !b.positioned {
		b.position(ctx)
		return false
	}
	evs, next, end, err := b.Harness.next(ctx, b.cursor)
	start := b.cursor.Count
	for i, e := range evs {
		if b.OnRaw != nil {
			b.OnRaw(e)
		}
		for k, m := range Map(e, b.RunID) {
			m = b.classified(m)
			b.push(ctx, wire.Item{Stream: wire.StreamEvents, Seq: SeqFor(start+int64(i)+1, k), Type: m.Type, Payload: m.Payload})
		}
	}
	b.cursor = next // Next advances over exactly the events it returned, error or not
	b.harnessRead(ctx, err)
	return end && err == nil
}

// readLog reads the harness log to its end, whatever the poll's backoff, until
// a read fails, the buffer is full or ctx ends (F11).
func (b *Bridge) readLog(ctx context.Context) {
	for ctx.Err() == nil {
		count := b.cursor.Count // positioning moves it too: Skip counts what it passes
		if b.pollEvents(ctx) || b.cursor.Count == count {
			return // at the log's end, or it did not move: a failed read, a full buffer
		}
	}
}

// classified sets a tool_call's class. An oversize stub is left as it is.
func (b *Bridge) classified(m Mapped) Mapped {
	if m.Type != envelope.ToolCall || b.Classify == nil {
		return m
	}
	var p envelope.ToolCallPayload
	var stub struct {
		Oversize bool `json:"oversize"`
	}
	if json.Unmarshal(m.Payload, &stub) != nil || stub.Oversize || json.Unmarshal(m.Payload, &p) != nil {
		return m
	}
	p.Class = b.Classify(p.Tool, p.Args, p.Risk)
	return fitted(envelope.ToolCall, envelope.Must(p))
}

// position rebuilds the cursor after a restart: the log holds events up to
// resumeAt, and the last event it touched is mapped again, since only some of
// its items may have landed (the store drops the rest). Skip rebuilds all three
// fields of the cursor a bridge that never stopped would hold (Ruling AL c).
// OnRaw still sees the skipped events, so the actions still pending are known.
func (b *Bridge) position(ctx context.Context) {
	n := b.resumeAt/ItemsPerEvent - 1
	if n <= 0 {
		b.positioned = true
		return
	}
	raw := b.OnRaw
	if raw == nil {
		raw = func(RawEvent) {}
	}
	c, err := b.Harness.skip(ctx, n, raw)
	b.harnessRead(ctx, err)
	if err == nil {
		b.cursor, b.positioned = c, true
	}
}

// harnessRead handles the outcome of a read of the harness log. A typed
// adapter error holds the cursor where it is: it is counted, logged at Error on
// every poll, told to the room once, and polled again with backoff; it is never
// skipped, since skipping would lose events or shift every later seq.
func (b *Bridge) harnessRead(ctx context.Context, err error) {
	now := b.now()
	reason := stallReason(err)
	switch {
	case err == nil:
		b.sawHarness(now)
		if b.stall != "" {
			b.log().Info("the harness log moves again", "reason", b.stall, "events", b.cursor.Count)
			b.stall = ""
		}
		b.pollRetry.reset()
		b.pollAt = time.Time{}
		return
	case reason != "":
		b.sawHarness(now) // it answered: restarting the bridge would not help
		b.stalls.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", reason)))
		b.log().Error("the harness log is stalled; the cursor holds and the bridge keeps polling",
			"reason", reason, "events", b.cursor.Count, "err", err)
		if b.stall != reason {
			b.stall = reason
			b.pushStatus(ctx, b.status.Emit(StallNotice(reason)))
		}
	case ctx.Err() != nil:
		return
	default:
		if se, ok := errors.AsType[*StatusError](err); ok && se.Code == http.StatusNotFound {
			b.log().Debug("the conversation does not exist yet")
		} else {
			b.log().Warn("read the harness events", "err", err)
		}
	}
	b.pollAt = now.Add(b.pollRetry.next(b.pollBackoffs()))
}

// pollStatus records the conversation's status and returns it, "" when the
// harness did not answer.
func (b *Bridge) pollStatus(ctx context.Context) string {
	status, err := b.Harness.Status(ctx)
	if err != nil {
		return ""
	}
	b.sawHarness(b.now())
	if b.bufBytes >= statusCap(b.MaxBuffer) {
		return status // a later poll records the status the harness settles on
	}
	changed := b.status.Observe(status, b.RunID)
	for _, it := range changed {
		b.pushStatus(ctx, it)
	}
	if len(changed) > 0 {
		// The events behind a change are in the log by now, and agent-run stops
		// agent-server soon after the conversation ends: read them at once (F11).
		b.readLog(ctx)
	}
	return status
}

func (b *Bridge) pushStatus(ctx context.Context, it StatusItem) {
	b.push(ctx, wire.Item{Stream: wire.StreamStatus, Seq: it.Seq, Type: it.Type, Payload: it.Payload})
}

// push buffers an item. One that could never fit a batch alone is stubbed now.
func (b *Bridge) push(ctx context.Context, it wire.Item) {
	p := encode(it)
	if len(p.enc)+batchOverhead(1) > maxBatchBytes {
		p = b.stubOf(ctx, p, StubOversize, 0, "")
	}
	b.buf = append(b.buf, p)
	b.bufBytes += len(p.enc)
}

// stubOf replaces p by its stub, counting and logging it by ids only.
func (b *Bridge) stubOf(ctx context.Context, p pending, why string, code int, reason string) pending {
	s := encodeStub(p, why, reason)
	b.stubbed.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", why)))
	b.log().Error("an item the broker cannot take is kept as a stub in its slot",
		"stream", p.stream, "seq", p.seq, "type", p.typ, "reason", why, "code", code, "brokerReason", reason)
	return s
}

// send re-acquires a lost lease first, then flushes.
func (b *Bridge) send(ctx context.Context) {
	if b.leaseLost {
		if _, _, ok := b.hello(ctx); !ok {
			return
		}
		b.leaseLost = false
		b.log().Info("the bridge lease is held again; appending resumes", "buffered", len(b.buf))
	}
	b.flush(ctx)
}

// heartbeatDue reports that the lease needs renewing although nothing is buffered.
func (b *Bridge) heartbeatDue() bool { return !b.now().Before(b.renewedAt.Add(heartbeatEvery)) }

// flush pushes the buffer in batches until it is empty or the broker refuses,
// and an empty batch when the lease is due a renewal: the broker renews it on
// every batch, fenced like an append. No batch is ever dropped: a 413 or 400
// halves the batch, down to a lone item that is then stubbed in its slot; a 409
// keeps the buffer until hello takes the lease back; anything else is retried
// after a backoff or Retry-After.
func (b *Bridge) flush(ctx context.Context) {
	limit := batchItems
	for (len(b.buf) > 0 || b.heartbeatDue()) && !b.sealed.Load() {
		n := nextBatch(b.buf, maxBatchBytes, limit)
		rep, err := b.Broker.Send(ctx, encoded(b.buf[:n]))
		switch {
		case err != nil && ctx.Err() != nil:
			return // cut short by SIGTERM: not the broker's failure, so no backoff
		case err != nil:
			b.log().Warn("push a batch", "items", n, "err", err)
			b.sendAt = b.now().Add(b.retryIn(rep))
			return
		case rep.Code == http.StatusOK:
			b.sendRetry.reset()
			b.renewedAt = b.now()
			limit = min(2*limit, batchItems) // grow back after a halving
			for _, p := range b.buf[:n] {
				b.bufBytes -= len(p.enc)
			}
			b.buf = b.buf[n:]
		case n > 0 && (rep.Code == http.StatusRequestEntityTooLarge || rep.Code == http.StatusBadRequest):
			if n > 1 {
				limit = n / 2
				continue
			}
			why := StubRefused
			if rep.Code == http.StatusRequestEntityTooLarge {
				why = StubOversize
			}
			if b.buf[0].stub != "" {
				// Even its stub is refused: nothing smaller exists. Stall loudly.
				b.log().Error("the broker refuses an item's stub; retrying", "stream", b.buf[0].stream,
					"seq", b.buf[0].seq, "code", rep.Code, "reason", rep.Reason)
				b.sendAt = b.now().Add(b.retryIn(rep))
				return
			}
			old := len(b.buf[0].enc)
			b.buf[0] = b.stubOf(ctx, b.buf[0], why, rep.Code, rep.Reason)
			b.bufBytes += len(b.buf[0].enc) - old
		case rep.Code == http.StatusConflict:
			// Ruling Y: another run took the room's lease. Stop appending, keep
			// the buffer and the cursor, and say hello until the lease is back.
			b.leaseLost = true
			b.log().Warn("the bridge lease was lost; appending stops until hello takes it back",
				"reason", rep.Reason, "buffered", len(b.buf))
			b.sendAt = b.now().Add(b.retryIn(rep))
			return
		case rep.Code == http.StatusGone:
			b.seal()
			return
		default:
			// 401 re-reads the token, 403 waits for the run watch, 429 and 5xx
			// wait for the broker.
			b.log().Warn("batch not accepted; retrying", "code", rep.Code, "reason", rep.Reason, "items", n)
			b.sendAt = b.now().Add(b.retryIn(rep))
			return
		}
	}
}

// shutdown drains the buffer within FlushGrace, then reads what the harness
// log has left to its end, if agent-run has not stopped it yet, and drains that
// too (review I1, F11).
// The loop's backoff does not carry over: the buffer is sent first and at once,
// before a poll that a hung harness could hold for its whole timeout, and each
// wait after a refusal is capped at drainWait.
func (b *Bridge) shutdown() {
	ctx, cancel := context.WithTimeout(context.Background(), b.FlushGrace)
	defer cancel()
	if b.sealed.Load() {
		return
	}
	b.sendAt = time.Time{}
	b.sendRetry.reset()
	b.takeInbox(ctx)
	b.drain(ctx)
	if ctx.Err() == nil && !b.sealed.Load() {
		b.readLog(ctx) // it positions the cursor first if the loop never could
		b.pollStatus(ctx)
		b.drain(ctx)
	}
	if len(b.buf) > 0 && !b.sealed.Load() {
		b.log().Warn("shutdown left items unmirrored; a restarted bridge resumes from the log",
			"items", len(b.buf), "leaseLost", b.leaseLost)
	}
}

// drain sends until the buffer is empty, the room is sealed or ctx ends.
func (b *Bridge) drain(ctx context.Context) {
	for len(b.buf) > 0 && !b.sealed.Load() {
		if pause(ctx, min(b.sendAt.Sub(b.now()), drainWait)) != nil {
			return
		}
		b.send(ctx)
	}
}

// consume keeps the broker's SSE stream open and dispatches its frames
// (phases 4 and 5).
func (b *Bridge) consume(ctx context.Context) {
	var retry backoff
	for {
		err := b.Broker.Stream(ctx, func(event string, data []byte) error { return b.dispatch(ctx, event, data) })
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			retry.reset() // a stream that ran its life: re-dial with a fresh token
		}
		b.log().Info("the broker stream ended; re-dialling", "err", err)
		if pause(ctx, retry.next(b.backoffs())) != nil {
			return
		}
	}
}

func (b *Bridge) dispatch(ctx context.Context, event string, data []byte) error {
	switch event {
	case wire.EventDeliver:
		var d wire.Deliver
		if json.Unmarshal(data, &d) == nil && b.OnDeliver != nil {
			return b.OnDeliver(ctx, d)
		}
	case wire.EventInterrupt:
		var i wire.Interrupt
		if json.Unmarshal(data, &i) == nil && b.OnInterrupt != nil {
			return b.OnInterrupt(ctx, i)
		}
	case wire.EventDecision:
		var d wire.Decision
		if json.Unmarshal(data, &d) == nil && b.OnDecision != nil {
			return b.OnDecision(ctx, d)
		}
	}
	return nil
}
