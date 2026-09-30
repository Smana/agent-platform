// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

type note struct {
	room string
	seq  int64
}

// listen runs s.Listen until the test ends and returns its notifications and its
// result. It waits for LISTEN to be in place.
func listen(t *testing.T, s *Store) (<-chan note, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	notes, ready := make(chan note, 256), make(chan struct{})
	done, finished := make(chan error, 1), make(chan struct{})
	go func() {
		defer close(finished)
		done <- s.Listen(ctx, func() { close(ready) }, func(room string, seq int64) { notes <- note{room, seq} })
	}()
	t.Cleanup(func() { cancel(); <-finished })
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("listen: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("LISTEN not in place after 5 s")
	}
	return notes, done
}

func expectNote(t *testing.T, notes <-chan note, want note) {
	t.Helper()
	select {
	case got := <-notes:
		if got != want {
			t.Fatalf("notified %+v, want %+v", got, want)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("no notification, want %+v", want)
	}
}

func expectNone(t *testing.T, notes <-chan note) {
	t.Helper()
	select {
	case got := <-notes:
		t.Fatalf("notified %+v, want nothing", got)
	case <-time.After(300 * time.Millisecond):
	}
}

// Ruling AT: the NOTIFY is part of the append's transaction, so PostgreSQL delivers
// it on commit only, and never for a rollback or an idempotent replay.
func TestNotifyFollowsCommit(t *testing.T) {
	s, _, _, _ := open(t)
	notes, _ := listen(t, s)
	ctx := t.Context()

	if _, _, err := s.Append(ctx, draft("agent:w", 1)); err != nil {
		t.Fatal(err)
	}
	expectNote(t, notes, note{room, 1})

	t.Run("a rolled-back append notifies nothing", func(t *testing.T) {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.appendTx(ctx, tx, draft("agent:w", 2), ""); err != nil {
			t.Fatal(err)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatal(err)
		}
		expectNone(t, notes)
	})
	t.Run("an append notifies at commit, not before", func(t *testing.T) {
		tx, err := s.pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, _, err := s.appendTx(ctx, tx, draft("agent:w", 2), ""); err != nil {
			t.Fatal(err)
		}
		expectNone(t, notes)
		if err := tx.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		expectNote(t, notes, note{room, 2})
	})
	t.Run("an idempotent replay notifies nothing", func(t *testing.T) {
		if _, dup, err := s.Append(ctx, draft("agent:w", 2)); err != nil || !dup {
			t.Fatalf("dup %v, %v", dup, err)
		}
		expectNone(t, notes)
	})
	t.Run("closing notifies the sealing event", func(t *testing.T) {
		if err := s.CloseRoom(ctx, room, "done"); err != nil {
			t.Fatal(err)
		}
		expectNote(t, notes, note{room, 3})
	})
}

// An append that fills the room also seals it: the last notification names the
// sealing event, the room's final seq.
func TestSealingAppendNotifiesTheLastSeq(t *testing.T) {
	s, _, _, _ := open(t)
	s.MaxEvents = 3
	notes, _ := listen(t, s)
	for n := int64(1); n <= 2; n++ {
		if _, _, err := s.Append(t.Context(), draft("agent:w", n)); err != nil {
			t.Fatal(err)
		}
	}
	for _, want := range []note{{room, 1}, {room, 2}, {room, 3}} {
		expectNote(t, notes, want)
	}
	if st, err := s.Room(t.Context(), room); err != nil || !st.Sealed || st.LastSeq != 3 {
		t.Fatalf("%+v %v", st, err)
	}
}

// A quiet room outlives the wait timeout: the listener pings and keeps listening.
func TestListenSurvivesAQuietSpell(t *testing.T) {
	s, _, _, _ := open(t)
	s.ListenPing = 50 * time.Millisecond
	notes, done := listen(t, s)
	time.Sleep(300 * time.Millisecond) // six timeouts
	select {
	case err := <-done:
		t.Fatalf("listen ended on a quiet connection: %v", err)
	default:
	}
	if _, _, err := s.Append(t.Context(), draft("agent:w", 1)); err != nil {
		t.Fatal(err)
	}
	expectNote(t, notes, note{room, 1})
}

func TestListenEnds(t *testing.T) {
	t.Run("with an error when its backend dies", func(t *testing.T) {
		s, _, _, super := open(t)
		_, done := listen(t, s)
		terminateListener(t, super)
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("a lost connection must be an error, so the caller reconnects")
			}
		case <-time.After(5 * time.Second):
			t.Fatal("listen outlived its backend")
		}
	})
	t.Run("cleanly with its context", func(t *testing.T) {
		s, _, _, _ := open(t)
		ctx, cancel := context.WithCancel(t.Context())
		ready, done := make(chan struct{}), make(chan error, 1)
		go func() { done <- s.Listen(ctx, func() { close(ready) }, func(string, int64) {}) }()
		<-ready
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("listen outlived its context")
		}
	})
}

// terminateListener kills the LISTEN backend as the superuser would on a failover.
func terminateListener(t *testing.T, super string) {
	t.Helper()
	exec(t, super, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE application_name = '`+ListenApplication+`'`)
}

// The connection budget: 8 pool connections plus one listener per replica (Ruling AT).
func TestPoolMaxConns(t *testing.T) {
	s, broker, _, _ := open(t)
	if got := s.pool.Config().MaxConns; got != 8 {
		t.Fatalf("default pool_max_conns = %d, want 8", got)
	}
	s2, err := Open(t.Context(), broker+"&pool_max_conns=3")
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if got := s2.pool.Config().MaxConns; got != 3 {
		t.Fatalf("a URL's pool_max_conns = %d, want it kept at 3", got)
	}
}
