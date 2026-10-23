// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/Smana/agent-platform/internal/factory/config"

	"github.com/Smana/agent-platform/internal/factory/forge"
)

// AutoMergedLabel marks a pull request the merger App merged, by class (v2 D1): with
// RevertOfLabel, the breaker's and the daily cap's record on GitHub, which a rebuild that loses
// every Task keeps.
func AutoMergedLabel(class string) string { return "factory/auto-merged:" + class }

// RevertOfLabel marks the merger's revert PR of pull request n (v2 D1).
func RevertOfLabel(n int) string { return fmt.Sprintf("factory/revert-of:#%d", n) }

// MergedLabel marks a merged pull request of a class whoever merged it (the merger, a maintainer,
// Renovate): the monitor's record, so the breaker's window sees merges the factory did not issue.
func MergedLabel(class string) string { return "factory/merged:" + class }

// FollowUpOfLabel marks a pull request that fixes pull request n after it merged (MC8).
func FollowUpOfLabel(n int) string { return fmt.Sprintf("factory/follow-up-of:#%d", n) }

// RegressionLabel marks a merged pull request the monitor found regressed (MC7).
const RegressionLabel = "factory/monitor:regression"

// Merge is one merged pull request of a class as the breaker counts it: when it merged, and
// whether a revert of it was opened.
//
// FollowUp: a later merge fixed it (MC8). Regression: the monitor found one (MC7). Auto: the
// merger merged it, so it counts against the daily caps.
type Merge struct {
	Number     int
	At         time.Time
	Reverted   bool
	FollowUp   bool
	Regression bool
	Auto       bool
}

// mergeLedger is GitHub's labelled merges per class, read once per process: everything the
// factory merges or reverts after the read is on its Tasks, which the breaker unites with it.
// An unread ledger is an error, so no decision is made on a breaker that might be tripped.
type mergeLedger struct {
	mu     sync.Mutex
	byRepo map[string]map[string][]Merge // by repository, then class; a repository absent until read
}

// githubLedger reads the ledger at the first decision and keeps it. A class's newest
// merge.breaker.window merges are each asked for a revert: the window never reaches older ones,
// since a Task's merge only pushes them further out.
func (r *Reconciler) githubLedger(ctx context.Context, rc *config.Repository) (map[string][]Merge, error) {
	r.ledger.mu.Lock()
	defer r.ledger.mu.Unlock()
	if byClass, ok := r.ledger.byRepo[rc.Name]; ok {
		return byClass, nil
	}
	rg, err := r.merger(ctx).PullsWithLabel(ctx, RegressionLabel)
	if err != nil {
		return nil, fmt.Errorf("read the merge ledger: %w", err)
	}
	regressed := make(map[int]bool, len(rg))
	for _, p := range rg {
		regressed[p.Number] = true
	}
	byClass := make(map[string][]Merge, len(rc.Classes))
	for _, class := range slices.Sorted(maps.Keys(rc.Classes)) {
		auto, err := r.merger(ctx).PullsWithLabel(ctx, AutoMergedLabel(class))
		if err != nil {
			return nil, fmt.Errorf("read the merge ledger: %w", err)
		}
		all, err := r.merger(ctx).PullsWithLabel(ctx, MergedLabel(class))
		if err != nil {
			return nil, fmt.Errorf("read the merge ledger: %w", err)
		}
		var ms []Merge
		at := map[int]int{}
		for k, ps := range [][]forge.LabelledPull{auto, all} { // a merger merge carries both labels: one entry
			for _, p := range ps {
				if p.State != "MERGED" || p.MergedAt.IsZero() {
					continue
				}
				isAuto := k == 0 // listed under AutoMergedLabel
				if i, ok := at[p.Number]; ok {
					ms[i].Auto = ms[i].Auto || isAuto
					continue
				}
				at[p.Number] = len(ms)
				ms = append(ms, Merge{Number: p.Number, At: p.MergedAt, Auto: isAuto})
			}
		}
		newestFirst(ms)
		for i := range ms[:min(len(ms), rc.Merge.Breaker.Window)] {
			// Any revert PR counts, as a Task counts from the revert's opening: a rebuild must not
			// change the breaker's verdict.
			rv, err := r.merger(ctx).PullsWithLabel(ctx, RevertOfLabel(ms[i].Number))
			if err != nil {
				return nil, fmt.Errorf("read the merge ledger: %w", err)
			}
			ms[i].Reverted = len(rv) > 0
			// A fix in review is not yet a fix: only a merged follow-up counts.
			fu, err := r.merger(ctx).PullsWithLabel(ctx, FollowUpOfLabel(ms[i].Number))
			if err != nil {
				return nil, fmt.Errorf("read the merge ledger: %w", err)
			}
			ms[i].FollowUp = slices.ContainsFunc(fu, func(p forge.LabelledPull) bool { return p.State == "MERGED" })
			ms[i].Regression = regressed[ms[i].Number]
		}
		byClass[class] = ms
	}
	if r.ledger.byRepo == nil {
		r.ledger.byRepo = map[string]map[string][]Merge{}
	}
	r.ledger.byRepo[rc.Name] = byClass
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
		out[i].FollowUp, out[i].Regression = out[i].FollowUp || m.FollowUp, out[i].Regression || m.Regression
		out[i].Auto = out[i].Auto || m.Auto
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
	if err := r.merger(ctx).AddLabels(ctx, pr.Number, l); err != nil {
		r.log().Error("label the merge ledger", "pr", pr.Number, "label", l, "err", err)
	}
}
