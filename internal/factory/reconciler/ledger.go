// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/Smana/agent-platform/internal/factory/forge"
)

// AutoMergedLabel marks a pull request the merger App merged, by class (v2 D1): with
// RevertOfLabel, the breaker's and the daily cap's record on GitHub, which a rebuild that loses
// every Task keeps.
func AutoMergedLabel(class string) string { return "factory/auto-merged:" + class }

// RevertOfLabel marks the merger's revert PR of pull request n (v2 D1).
func RevertOfLabel(n int) string { return fmt.Sprintf("factory/revert-of:#%d", n) }

// Merge is one merged pull request of a class as the breaker counts it: when it merged, and
// whether a revert of it was opened.
type Merge struct {
	Number   int
	At       time.Time
	Reverted bool
}

// mergeLedger is GitHub's labelled merges per class, read once per process: everything the
// factory merges or reverts after the read is on its Tasks, which the breaker unites with it.
// An unread ledger is an error, so no decision is made on a breaker that might be tripped.
type mergeLedger struct {
	mu      sync.Mutex
	byClass map[string][]Merge // nil until read
}

// githubLedger reads the ledger at the first decision and keeps it. A class's newest
// merge.breaker.window merges are each asked for a revert: the window never reaches older ones,
// since a Task's merge only pushes them further out.
func (r *Reconciler) githubLedger(ctx context.Context) (map[string][]Merge, error) {
	r.ledger.mu.Lock()
	defer r.ledger.mu.Unlock()
	if r.ledger.byClass != nil {
		return r.ledger.byClass, nil
	}
	byClass := make(map[string][]Merge, len(r.Cfg.Classes))
	for _, class := range slices.Sorted(maps.Keys(r.Cfg.Classes)) {
		ps, err := r.Merger.PullsWithLabel(ctx, AutoMergedLabel(class))
		if err != nil {
			return nil, fmt.Errorf("read the merge ledger: %w", err)
		}
		var ms []Merge
		for _, p := range ps {
			if p.State == "MERGED" && !p.MergedAt.IsZero() {
				ms = append(ms, Merge{Number: p.Number, At: p.MergedAt})
			}
		}
		newestFirst(ms)
		for i := range ms[:min(len(ms), r.Cfg.Merge.Breaker.Window)] {
			// Any revert PR counts, as a Task counts from the revert's opening: a rebuild must not
			// change the breaker's verdict.
			rv, err := r.Merger.PullsWithLabel(ctx, RevertOfLabel(ms[i].Number))
			if err != nil {
				return nil, fmt.Errorf("read the merge ledger: %w", err)
			}
			ms[i].Reverted = len(rv) > 0
		}
		byClass[class] = ms
	}
	r.ledger.byClass = byClass
	r.log().Info("merge ledger read from GitHub", "classes", len(byClass))
	return byClass, nil
}

// uniteMerges is the Tasks' merges and GitHub's, one per pull request: a revert either side
// knows of counts, and GitHub's merge time wins. A merge without a number is kept as is.
func uniteMerges(tasks, github []Merge) []Merge {
	out := slices.Clone(tasks)
	at := make(map[int]int, len(out))
	for i, m := range out {
		if m.Number > 0 {
			at[m.Number] = i
		}
	}
	for _, m := range github {
		i, ok := at[m.Number]
		if !ok {
			out = append(out, m)
			continue
		}
		out[i].At, out[i].Reverted = m.At, out[i].Reverted || m.Reverted
	}
	return out
}

func newestFirst(ms []Merge) {
	slices.SortFunc(ms, func(x, y Merge) int { return y.At.Compare(x.At) })
}

// label adds l to the pull request through the merger App unless it already carries it. A
// failure is logged, not returned: the label is the record, and the merge or revert it records
// must not wait on it. Each caller re-tries on its next pass where it reads the pull request again.
func (r *Reconciler) label(ctx context.Context, pr forge.PR, l string) {
	if slices.Contains(pr.Labels, l) {
		return
	}
	if err := r.Merger.AddLabels(ctx, pr.Number, l); err != nil {
		r.log().Error("label the merge ledger", "pr", pr.Number, "label", l, "err", err)
	}
}
