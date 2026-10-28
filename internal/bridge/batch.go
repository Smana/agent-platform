// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Smana/maku/internal/envelope"
	"github.com/Smana/maku/internal/wire"
)

// pageItemBytes is the most one poll adds to the buffer: maxPages pages of
// pageLimit events, each mapped to at most two items.
func pageItemBytes(maxPages int) int { return max(maxPages, 1) * pageLimit * 2 * maxItemBytes }

// statusItemsPerPoll is the most items one status poll adds.
const statusItemsPerPoll = 2

// statusCap is where status items stop too (review N1). They pass MaxBuffer,
// since a transition missed is a turn lost, but a lease lost for good must not
// grow the buffer for as long as the pod lives. Events are mapped only below
// MaxBuffer, so the buffer holds at most MaxBuffer plus one poll's items, or
// this cap plus one poll's status items and one step's verdict records. The
// confirmation loop's read past MaxBuffer runs only below this cap too: its
// verdicts are records, and at the cap they wait, the harness parked, rather
// than grow the buffer or drop one.
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
