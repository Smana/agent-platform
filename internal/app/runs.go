// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	"github.com/Smana/maku/internal/runwatch"
)

// informerSource is the one cache method the watch needs; a manager's cache has it.
type informerSource interface {
	GetInformer(ctx context.Context, obj client.Object, opts ...cache.InformerGetOption) (cache.Informer, error)
}

// runWiring is the AgentRun watch and when it can be trusted as complete.
type runWiring struct {
	watch *runwatch.Watcher
	// synced waits until the watch's handler holds the informer's first list, or
	// ctx ends. The leader's sweep and /readyz wait on it (review I1).
	synced func(ctx context.Context) bool
}

// wireRuns attaches the watch to the AgentRun informer on every replica (bridge
// admission reads it) and adds, through add, the leader Runnable that alone
// appends run lifecycles: the watch's callbacks, a replay once elected, and the
// sweep of runs that joined and never left. tick paces the sweep; nil means a
// time.Ticker.
func wireRuns(ctx context.Context, src informerSource, add func(manager.Runnable) error, log *slog.Logger,
	open runwatch.Unfinished, events *runwatch.Events, tick func(time.Duration) (<-chan time.Time, func()),
) (runWiring, error) {
	watch := runwatch.New()
	handlerSynced, err := runwatch.Register(ctx, src, watch)
	if err != nil {
		return runWiring{}, err
	}
	synced := func(ctx context.Context) bool { return toolscache.WaitForCacheSync(ctx.Done(), handlerSynced) }
	var leading atomic.Bool
	watch.OnChange(bounded(&leading, log, "record a run's lifecycle", events.Observe))
	// A claim deleted mid-run never reaches a terminal phase in the watch (review M15).
	watch.OnRemove(bounded(&leading, log, "record a deleted run", events.ObserveDeleted))
	replay := bounded(&leading, log, "replay a run's lifecycle", events.Observe)
	if err := add(&leader{active: &leading, synced: synced, every: sweepEvery, ticker: tick,
		replay: func(ctx context.Context) {
			for _, r := range watch.All() {
				replay(ctx, r)
			}
		},
		sweep: func(ctx context.Context) {
			ctx, cancel := context.WithTimeout(ctx, sweepTimeout)
			defer cancel()
			if err := events.Sweep(ctx, open, watch.Get); err != nil {
				log.Error("sweep the runs that joined and never left", "err", err)
			}
		}}); err != nil {
		return runWiring{}, fmt.Errorf("leader: %w", err)
	}
	return runWiring{watch: watch, synced: synced}, nil
}

// bounded runs a lifecycle append for the leader only, bounded by observeTimeout,
// and logs its failure: the watch's callbacks return nothing, and the sweep
// retries what failed.
func bounded(leading *atomic.Bool, log *slog.Logger, msg string, f func(context.Context, runwatch.Run) error) func(context.Context, runwatch.Run) {
	return func(ctx context.Context, r runwatch.Run) {
		if !leading.Load() {
			return
		}
		ctx, cancel := context.WithTimeout(ctx, observeTimeout)
		defer cancel()
		if err := f(ctx, r); err != nil {
			log.Error(msg, "run", r.ID, "room", r.Room, "err", err)
		}
	}
}

// runLiveness is the one watch method runInfoWatch reads; *runwatch.Watcher has it.
type runLiveness interface {
	Live(id string) (run runwatch.Run, live, known bool)
}

// runInfoWatch is what rooms_run_info reads of a run: its claim's spec.model (runwatch.Run.Model,
// already bounded), and whether to keep it: while it is live, and while the watch has not synced
// and so cannot tell a run it lacks from a deleted one.
func runInfoWatch(w runLiveness) func(run string) (model string, live bool) {
	return func(run string) (string, bool) {
		r, live, known := w.Live(run)
		return r.Model, live || !known
	}
}
