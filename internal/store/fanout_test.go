// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/Smana/agent-platform/internal/fanout"
	"github.com/Smana/agent-platform/internal/store"
)

// Ruling AT end to end, on a real PostgreSQL: appends reach a viewer through
// LISTEN/NOTIFY; when the LISTEN backend is killed the hub polls, then reconnects
// and catches up, and the viewer sees every seq exactly once. A viewer that never
// reads stalls no append.
func TestHubOverPostgres(t *testing.T) {
	s, super := store.OpenForTest(t)
	h := fanout.New(s, s, slog.New(slog.DiscardHandler))
	h.PollEvery = 50 * time.Millisecond
	h.MinBackoff = 1500 * time.Millisecond // the poll must deliver before the reconnect
	h.Budget = 2000                        // about 40 of these events: the stalled viewer overflows it
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	waitFor(ctx, t, "the listener", h.Healthy)

	viewer, err := h.Subscribe(ctx, store.RoomForTest)
	if err != nil {
		t.Fatal(err)
	}
	stalled, err := h.Subscribe(ctx, store.RoomForTest) // never reads
	if err != nil {
		t.Fatal(err)
	}
	next := int64(1)
	appendAndSee := func(n int, phase string) {
		t.Helper()
		first := next
		for range n {
			start := time.Now()
			if _, _, err := s.Append(ctx, store.DraftForTest("agent:w", next)); err != nil {
				t.Fatal(err)
			}
			if d := time.Since(start); d > time.Second {
				t.Fatalf("%s: an append took %s", phase, d)
			}
			next++
		}
		for want := first; want < next; want++ {
			select {
			case e := <-viewer.C:
				viewer.Sent(e)
				if e.Seq != want {
					t.Fatalf("%s: seq %d, want %d: a gap or a duplicate", phase, e.Seq, want)
				}
			case <-ctx.Done():
				t.Fatalf("%s: stuck before seq %d", phase, want)
			}
		}
	}

	appendAndSee(20, "notified")
	store.TerminateListener(t, super)
	waitFor(ctx, t, "the listener to drop", func() bool { return !h.Healthy() })
	appendAndSee(20, "polled")
	if h.Healthy() {
		t.Fatal("reconnected before the poll delivered: the test did not isolate the poll")
	}
	waitFor(ctx, t, "the reconnect", h.Healthy)
	appendAndSee(20, "notified again")
	select {
	case e := <-viewer.C:
		t.Fatalf("seq %d delivered twice", e.Seq)
	case <-time.After(200 * time.Millisecond):
	}
	select {
	case <-stalled.Dropped:
	default:
		t.Fatal("the viewer that never read was not dropped")
	}
}

func waitFor(ctx context.Context, t *testing.T, what string, ok func() bool) {
	t.Helper()
	for !ok() {
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for %s: %v", what, ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
}
