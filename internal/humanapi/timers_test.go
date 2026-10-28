// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"testing"
	"time"

	"github.com/Smana/maku/internal/fanout"
)

// fakeTimers is a Server.Timer the test steps by hand: a timer of the ping period lands on
// pings, any other (the lifetime cap) on caps, and none fires until the test says so.
type fakeTimers struct {
	ping        time.Duration
	pings, caps chan *fakeTimer
}

type fakeTimer struct {
	d time.Duration
	c chan time.Time
}

func newFakeTimers(ping time.Duration) *fakeTimers {
	return &fakeTimers{ping: ping, pings: make(chan *fakeTimer, 64), caps: make(chan *fakeTimer, 64)}
}

func (f *fakeTimers) option(s *Server, _ *fanout.Hub, _ *hubView) {
	s.PingEvery = f.ping
	s.Timer = func(d time.Duration) (<-chan time.Time, func() bool) {
		ft := &fakeTimer{d: d, c: make(chan time.Time, 1)}
		if d == f.ping {
			f.pings <- ft
		} else {
			f.caps <- ft
		}
		return ft.c, func() bool { return true }
	}
}

func take(t *testing.T, from <-chan *fakeTimer, what string) *fakeTimer {
	t.Helper()
	select {
	case ft := <-from:
		return ft
	case <-time.After(5 * time.Second):
		t.Fatalf("no %s timer was started", what)
		return nil
	}
}

// beat fires the pending ping timer and returns once the loop has started the next one,
// that is once the beat's work (the D7 re-check, the ping) is done.
func (f *fakeTimers) beat(t *testing.T) {
	t.Helper()
	take(t, f.pings, "ping").c <- time.Time{}
	f.pings <- take(t, f.pings, "next ping") // peeked: hand it back for the next beat
}
