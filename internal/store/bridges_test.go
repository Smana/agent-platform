// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func alive(context.Context, string) bool { return true }
func ended(context.Context, string) bool { return false }

// Ruling AE: an empty bridge run would skip the lease fence and append as an
// unfenced writer. It is refused before the database is touched (the zero
// Store has no pool: reaching it would panic).
func TestAppendAsBridgeRefusesAnEmptyRun(t *testing.T) {
	_, _, err := (&Store{}).AppendAsBridge(t.Context(), "", draft("agent:7f3cq2xz", 1))
	if !errors.Is(err, ErrNoBridgeRun) {
		t.Fatalf("err = %v, want ErrNoBridgeRun", err)
	}
}

// Ruling P17 across replicas (review I7): the lease lives in the room's row.
func TestBridgeLeaseIsSharedAndExpires(t *testing.T) {
	s, _, _, _ := open(t)
	ctx := context.Background()
	for _, step := range []struct {
		name       string
		run        string
		stale      time.Duration
		live       func(context.Context, string) bool
		wantOK     bool
		wantHolder string
	}{
		{"the first claim takes the lease", "7f3cq2xz", 2 * time.Minute, alive, true, "7f3cq2xz"},
		{"a second run cannot take a fresh lease", "aaaaaaaa", 2 * time.Minute, alive, false, "7f3cq2xz"},
		{"the holder re-claims its own lease", "7f3cq2xz", 2 * time.Minute, alive, true, "7f3cq2xz"},
		{"a run that ended frees the lease at once", "aaaaaaaa", 2 * time.Minute, ended, true, "aaaaaaaa"},
		{"a stale lease is free", "7f3cq2xz", 0, alive, true, "7f3cq2xz"},
	} {
		holder, ok, err := s.ClaimBridge(ctx, room, step.run, step.stale, step.live)
		if err != nil || ok != step.wantOK || holder != step.wantHolder {
			t.Fatalf("%s: holder=%s ok=%v err=%v", step.name, holder, ok, err)
		}
	}
}

// live may call the Kubernetes API: it must never run while the room's row is locked.
func TestClaimBridgeAsksLivenessOutsideTheRowLock(t *testing.T) {
	s, _, _, _ := open(t)
	ctx := context.Background()
	if _, ok, err := s.ClaimBridge(ctx, room, "7f3cq2xz", time.Minute, alive); err != nil || !ok {
		t.Fatalf("first claim: %v %v", ok, err)
	}
	var lockErr error
	probe := func(ctx context.Context, _ string) bool {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			lockErr = err
			return true
		}
		defer func() { _ = tx.Rollback(ctx) }()
		_, lockErr = tx.Exec(ctx, `SELECT 1 FROM rooms WHERE room_id = $1 FOR UPDATE NOWAIT`, room)
		return true
	}
	if _, _, err := s.ClaimBridge(ctx, room, "aaaaaaaa", time.Minute, probe); err != nil {
		t.Fatal(err)
	}
	if lockErr != nil {
		t.Fatalf("live ran under the row lock: %v", lockErr)
	}
}

// A displaced bridge learns it from TouchBridge, and its appends are fenced off.
func TestDisplacedBridgeIsFenced(t *testing.T) {
	s, _, _, _ := open(t)
	ctx := context.Background()
	if _, ok, err := s.ClaimBridge(ctx, room, "7f3cq2xz", time.Minute, alive); err != nil || !ok {
		t.Fatalf("A claims: %v %v", ok, err)
	}
	if _, _, err := s.AppendAsBridge(ctx, "7f3cq2xz", draft("agent:7f3cq2xz", 1)); err != nil {
		t.Fatalf("the holder appends: %v", err)
	}
	if _, ok, err := s.ClaimBridge(ctx, room, "aaaaaaaa", 0, alive); err != nil || !ok {
		t.Fatalf("B takes the stale lease: %v %v", ok, err)
	}
	for _, tc := range []struct {
		run      string
		wantHeld bool
	}{{"7f3cq2xz", false}, {"aaaaaaaa", true}} {
		if held, err := s.TouchBridge(ctx, room, tc.run); err != nil || held != tc.wantHeld {
			t.Fatalf("TouchBridge(%s) = %v, %v; want %v", tc.run, held, err, tc.wantHeld)
		}
	}
	before, err := s.Room(ctx, room)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.AppendAsBridge(ctx, "7f3cq2xz", draft("agent:7f3cq2xz", 2)); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("the displaced bridge appends: want ErrLeaseLost, got %v", err)
	}
	if after, _ := s.Room(ctx, room); after.LastSeq != before.LastSeq {
		t.Fatalf("a fenced append wrote: last_seq %d -> %d", before.LastSeq, after.LastSeq)
	}
	if _, _, err := s.AppendAsBridge(ctx, "aaaaaaaa", draft("agent:aaaaaaaa", 1)); err != nil {
		t.Fatalf("the new holder appends: %v", err)
	}
}

// Review I6: a value PostgreSQL refuses is a data error, never "the log is down".
func TestNULIsADataError(t *testing.T) {
	s, _, _, _ := open(t)
	d := draft("agent:x", 1)
	d.Payload = []byte(`{"output":"a\u0000b"}`)
	if _, _, err := s.Append(context.Background(), d); !IsDataError(err) {
		t.Fatalf("want a class-22 error, got %v", err)
	}
}
