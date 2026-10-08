// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"sync/atomic"
	"time"
)

// leader is a manager Runnable only the elected replica runs. It marks this
// replica as the one that appends run lifecycles, replays every watched run (a
// new leader writes nothing twice: every key is fixed), then sweeps the runs that
// joined and never left, at once and every period.
type leader struct {
	active *atomic.Bool
	// synced waits for the watch's first list; false means ctx ended first.
	synced func(ctx context.Context) bool
	replay func(ctx context.Context)
	sweep  func(ctx context.Context)
	every  time.Duration
	// ticker paces the sweep; nil means a time.Ticker.
	ticker func(d time.Duration) (c <-chan time.Time, stop func())
}

// Start runs until ctx ends, which is when this replica stops leading.
func (l *leader) Start(ctx context.Context) error {
	// Before the sync: an event the informer delivers meanwhile is appended by
	// its callback, and the replay covers what came earlier.
	l.active.Store(true)
	defer l.active.Store(false)
	// The sweep ends every joined run the watch does not know: before the first
	// list, that would be every run.
	if !l.synced(ctx) {
		return nil
	}
	l.replay(ctx)
	l.sweep(ctx)
	tick, stop := l.tick(l.every)
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick:
			l.sweep(ctx)
		}
	}
}

// NeedLeaderElection makes the manager run it on the leader only.
func (*leader) NeedLeaderElection() bool { return true }

func (l *leader) tick(d time.Duration) (<-chan time.Time, func()) { return newTicker(l.ticker, d) }

// leaderLoop is a periodic job only the elected replica runs; phase 4's lease
// sweeper and phase 5's expiry sweeper reuse it. active is true while it runs,
// so a job can check the lease before each side effect (ruling SY).
type leaderLoop struct {
	every  time.Duration
	run    func(ctx context.Context)
	active *atomic.Bool
	// ticker paces the job; nil means a time.Ticker.
	ticker func(d time.Duration) (c <-chan time.Time, stop func())
	// done, when set, runs once this replica stops leading: a gauge only the
	// leader sets must not keep a former leader's value.
	done func()
}

// Start runs the job on every tick until ctx ends, which is when this replica
// stops leading.
func (l *leaderLoop) Start(ctx context.Context) error {
	l.active.Store(true)
	defer l.active.Store(false)
	if l.done != nil {
		defer l.done()
	}
	tick, stop := newTicker(l.ticker, l.every)
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick:
			l.run(ctx)
		}
	}
}

// NeedLeaderElection makes the manager run it on the leader only.
func (*leaderLoop) NeedLeaderElection() bool { return true }

// newTicker is ticker's, or a time.Ticker's.
func newTicker(ticker func(time.Duration) (<-chan time.Time, func()), d time.Duration) (<-chan time.Time, func()) {
	if ticker != nil {
		return ticker(d)
	}
	t := time.NewTicker(d)
	return t.C, t.Stop
}
