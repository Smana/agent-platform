// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"context"
	"time"
)

// FinalReadResult is what a final read reached: the harness events read so far, the items still
// unmirrored when it answered, and whether the room is sealed.
type FinalReadResult struct {
	Events     int64 `json:"events"`
	Unmirrored int   `json:"unmirrored"`
	Sealed     bool  `json:"sealed"`
}

// finalRead is one FinalRead waiting for the loop.
type finalRead struct {
	ctx  context.Context
	done chan FinalReadResult
}

// answerMargin is how long before the caller's deadline a final read stops working, so its answer
// still reaches the caller in time.
const answerMargin = 250 * time.Millisecond

func (b *Bridge) finals() chan finalRead {
	b.finalOnce.Do(func() { b.finalReads = make(chan finalRead) })
	return b.finalReads
}

// FinalRead asks the loop to read the harness log to its end and mirror it now, and waits for the
// answer or ctx. agent-run calls it on SIGTERM before it stops agent-server (F11, the harness half;
// disruption design §2): the bridge's own SIGTERM drain comes only once the harness has exited, when
// agent-server is gone. It does not stop the loop.
func (b *Bridge) FinalRead(ctx context.Context) (FinalReadResult, error) {
	req := finalRead{ctx: ctx, done: make(chan FinalReadResult, 1)}
	select {
	case b.finals() <- req:
	case <-ctx.Done():
		return FinalReadResult{}, ctx.Err()
	}
	select {
	case res := <-req.done:
		return res, nil
	case <-ctx.Done():
		return FinalReadResult{}, ctx.Err()
	}
}

// ticker is Ticker, or a one-shot timer: the loop waits one period per step, and a step's own
// duration is part of the period, as before.
func (b *Bridge) ticker(d time.Duration) (<-chan time.Time, func()) {
	if b.Ticker != nil {
		return b.Ticker(d)
	}
	t := time.NewTimer(d)
	return t.C, func() { t.Stop() }
}

// idle waits out the poll interval, serving on the loop, the only goroutine that touches the buffer
// and the cursor, the final reads that arrive meanwhile. It reports false once ctx ends.
func (b *Bridge) idle(ctx context.Context) bool {
	tick, stop := b.ticker(b.Interval)
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case req := <-b.finals():
			req.done <- b.serveFinalRead(req.ctx)
		case <-tick:
			return true
		}
	}
}

// serveFinalRead reads the log to its end, then the status, then sends what that buffered. The
// loop's backoff does not apply, as in shutdown, and it stops answerMargin before ctx's deadline.
func (b *Bridge) serveFinalRead(ctx context.Context) FinalReadResult {
	if d, ok := ctx.Deadline(); ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, d.Add(-answerMargin))
		defer cancel()
	}
	if !b.sealed.Load() {
		b.sendAt = time.Time{}
		b.sendRetry.reset()
		b.takeInbox(ctx)
		b.readLog(ctx)
		b.pollStatus(ctx)
		b.drain(ctx)
	}
	return FinalReadResult{Events: b.cursor.Count, Unmirrored: len(b.buf), Sealed: b.sealed.Load()}
}
