// SPDX-License-Identifier: Apache-2.0

package store

import (
	"slices"
	"testing"
	"time"
)

// appendN gives roomID n events, so its last_seq is n.
func appendN(t *testing.T, s *Store, roomID string, n int64) {
	t.Helper()
	for i := int64(1); i <= n; i++ {
		if _, _, err := s.Append(t.Context(), draftIn(roomID, "agent:x", i)); err != nil {
			t.Fatal(err)
		}
	}
}

func cursorOf(t *testing.T, s *Store, sub, roomID string) int64 {
	t.Helper()
	seq, err := s.WebhookCursor(t.Context(), sub, roomID)
	if err != nil {
		t.Fatal(err)
	}
	return seq
}

// R18, B3: a subscriber's first registration puts every existing room's cursor at its last seq;
// a second call and a new generation add none and keep the old ones; a room created afterwards
// has no cursor and starts at its first event; a cursor never goes back.
func TestWebhookCursors(t *testing.T) {
	ctx := t.Context()
	s, _, _, _ := open(t)
	appendN(t, s, room, 3)
	if _, _, err := s.Subscriber(ctx, "slack-app", 0); err != nil {
		t.Fatal(err)
	}
	if got := cursorOf(t, s, "slack-app", room); got != 3 {
		t.Fatalf("first registration: cursor %d, want the room's last seq 3", got)
	}
	if err := s.SetWebhookCursor(ctx, "slack-app", room, 2); err != nil || cursorOf(t, s, "slack-app", room) != 3 {
		t.Fatalf("a cursor never goes back: %v", err)
	}
	if err := s.SetWebhookCursor(ctx, "slack-app", room, 5); err != nil || cursorOf(t, s, "slack-app", room) != 5 {
		t.Fatalf("a cursor moves forward: %v", err)
	}
	if _, err := s.EnsureRoom(ctx, NewRoom{ID: "latecome", Driver: "system:factory", Retention: 90 * 24 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	appendN(t, s, "latecome", 2)
	if got := cursorOf(t, s, "slack-app", "latecome"); got != 0 {
		t.Fatalf("a room created after the subscriber: cursor %d, want 0", got)
	}
	for _, gen := range []int{0, 1} { // the same call again, then a new generation
		if _, _, err := s.Subscriber(ctx, "slack-app", gen); err != nil {
			t.Fatal(err)
		}
		if got := cursorOf(t, s, "slack-app", room); got != 5 {
			t.Fatalf("generation %d: the old cursor is %d, want 5", gen, got)
		}
		if got := cursorOf(t, s, "slack-app", "latecome"); got != 0 {
			t.Fatalf("generation %d: a cursor was added for a room that had none: %d", gen, got)
		}
	}
	// A second subscriber is registered on its own: it too starts at each room's last seq.
	if _, _, err := s.Subscriber(ctx, "other", 0); err != nil {
		t.Fatal(err)
	}
	if got := cursorOf(t, s, "other", "latecome"); got != 2 {
		t.Fatalf("another subscriber's first registration: cursor %d, want 2", got)
	}
}

// R17: failures count, a success resets them, a disabled subscriber stays disabled until its
// generation changes, which resets the failures and re-enables it.
func TestWebhookSubscriberState(t *testing.T) {
	ctx := t.Context()
	s, _, _, _ := open(t)
	if f, d, err := s.Subscriber(ctx, "slack-app", 0); err != nil || f != 0 || d {
		t.Fatal(f, d, err)
	}
	for want := 1; want <= 2; want++ {
		if got, err := s.Failed(ctx, "slack-app"); err != nil || got != want {
			t.Fatalf("failure %d: %d, %v", want, got, err)
		}
	}
	if err := s.Succeeded(ctx, "slack-app"); err != nil {
		t.Fatal(err)
	}
	if f, _, err := s.Subscriber(ctx, "slack-app", 0); err != nil || f != 0 {
		t.Fatalf("a success resets the streak: %d, %v", f, err)
	}
	if _, err := s.Failed(ctx, "slack-app"); err != nil {
		t.Fatal(err)
	}
	if err := s.Disable(ctx, "slack-app"); err != nil {
		t.Fatal(err)
	}
	if f, d, err := s.Subscriber(ctx, "slack-app", 0); err != nil || f != 1 || !d {
		t.Fatalf("the same generation stays disabled: %d %v %v", f, d, err)
	}
	if f, d, err := s.Subscriber(ctx, "slack-app", 1); err != nil || f != 0 || d {
		t.Fatalf("a new generation resets failures and re-enables: %d %v %v", f, d, err)
	}
}

// Lagging lists the rooms past a subscriber's cursor, a room with no cursor included, and not a
// room at its cursor.
func TestWebhookLagging(t *testing.T) {
	ctx := t.Context()
	s, _, _, _ := open(t)
	appendN(t, s, room, 2)
	if _, _, err := s.Subscriber(ctx, "slack-app", 0); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Lagging(ctx, "slack-app", 10); err != nil || len(got) != 0 {
		t.Fatalf("a room at its cursor does not lag: %v %v", got, err)
	}
	for _, id := range []string{"laggingx", "laggingy"} {
		if _, err := s.EnsureRoom(ctx, NewRoom{ID: id, Driver: "system:factory", Retention: 90 * 24 * time.Hour}); err != nil {
			t.Fatal(err)
		}
		appendN(t, s, id, 1)
	}
	appendN(t, s, room, 3) // events 1 and 2 again are idempotent: only seq 3 is new
	got, err := s.Lagging(ctx, "slack-app", 10)
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(got)
	want := []string{"laggingx", "laggingy", room}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("lagging = %v, want %v", got, want)
	}
	if err := s.SetWebhookCursor(ctx, "slack-app", "laggingx", 1); err != nil {
		t.Fatal(err)
	}
	if got, err := s.Lagging(ctx, "slack-app", 1); err != nil || len(got) != 1 {
		t.Fatalf("the limit bounds the list: %v %v", got, err)
	}
	if got, _ := s.Lagging(ctx, "slack-app", 10); slices.Contains(got, "laggingx") {
		t.Fatalf("a room at its cursor does not lag: %v", got)
	}
}

// M6: a purged room drops its cursors first, and the foreign key does not fail the purge.
func TestPurgeDropsWebhookCursors(t *testing.T) {
	ctx := t.Context()
	s, _, retention, super := open(t)
	if _, err := s.EnsureRoom(ctx, NewRoom{ID: "expiredx", Driver: "system:factory", Retention: 90 * 24 * time.Hour}); err != nil {
		t.Fatal(err)
	}
	appendN(t, s, "expiredx", 2)
	if _, _, err := s.Subscriber(ctx, "slack-app", 0); err != nil {
		t.Fatal(err)
	}
	if err := s.CloseRoom(ctx, "expiredx", "done"); err != nil {
		t.Fatal(err)
	}
	forge(t, super, `UPDATE rooms SET closed_at = now() - interval '91 days' WHERE room_id = 'expiredx'`)
	r, err := Open(ctx, retention)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if rooms, _, err := r.PurgeExpired(ctx); err != nil || rooms != 1 {
		t.Fatalf("purged %d rooms: %v", rooms, err)
	}
	if got := cursorOf(t, s, "slack-app", "expiredx"); got != 0 {
		t.Fatalf("the purged room's cursor is %d", got)
	}
}
