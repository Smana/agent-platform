package store

import (
	"context"
	"testing"
	"time"
)

// Ruling P17 across replicas (review I7): the lease lives in the room's row.
func TestBridgeLeaseIsSharedAndExpires(t *testing.T) {
	s, _, _, _ := open(t)
	ctx := context.Background()
	live := func(string) bool { return true }
	if _, ok, err := s.ClaimBridge(ctx, room, "7f3cq2xz", 2*time.Minute, live); err != nil || !ok {
		t.Fatalf("first claim: %v %v", ok, err)
	}
	if holder, ok, _ := s.ClaimBridge(ctx, room, "aaaaaaaa", 2*time.Minute, live); ok || holder != "7f3cq2xz" {
		t.Fatalf("a second run took a fresh lease: %v %s", ok, holder)
	}
	if _, ok, _ := s.ClaimBridge(ctx, room, "7f3cq2xz", 2*time.Minute, live); !ok {
		t.Fatal("the holder re-claims its own lease")
	}
	if _, ok, _ := s.ClaimBridge(ctx, room, "aaaaaaaa", 2*time.Minute, func(string) bool { return false }); !ok {
		t.Fatal("a run that ended frees the lease at once")
	}
	if _, ok, _ := s.ClaimBridge(ctx, room, "7f3cq2xz", 0, live); !ok {
		t.Fatal("a stale lease is free")
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
