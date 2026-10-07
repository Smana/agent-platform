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

func (l *leader) tick(d time.Duration) (<-chan time.Time, func()) {
	if l.ticker != nil {
		return l.ticker(d)
	}
	t := time.NewTicker(d)
	return t.C, t.Stop
}
