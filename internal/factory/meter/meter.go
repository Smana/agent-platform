// SPDX-License-Identifier: Apache-2.0

// Package meter is the run meter (§4, C3, C5): every poll.meter, on every run, it writes the
// usage annotation the composition projects into status.usage.tokens, and revokes a live run at
// its own cap (budget-run, enforced from phase 1: R3). Crossplane owns AgentRun status; this
// writes annotations only. Every run is metered, human-launched ones included.
package meter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/Smana/agent-platform/internal/factory/runs"
)

// ReasonBudgetRun is the revocation at a run's own cap; the composition turns it into
// BudgetExhausted, and the broker's end reason reads it back (runwatch.EndReason).
const ReasonBudgetRun = "budget-run"

// Store is the part of the AgentRun client the meter uses; runs.Client implements it.
type Store interface {
	List(ctx context.Context) ([]runs.Run, error)
	Annotate(ctx context.Context, id string, kv map[string]string) error
}

// Source reads each run's raw token counter, keyed by run id; VM implements it.
type Source interface {
	RunTokens(ctx context.Context) (map[string]int64, error)
}

// Meter is a leader-only manager.Runnable. Tick is not safe for concurrent use: Start is its one
// caller in the binary.
type Meter struct {
	Runs   Store
	Source Source
	Every  time.Duration
	// OnRevoke is told of each revocation written, with its reason: fmetrics.Set.Revoked.
	OnRevoke func(ctx context.Context, reason string)
	// Ticker starts the period; nil means a time.Ticker.
	Ticker func(d time.Duration) (c <-chan time.Time, stop func())
	Log    *slog.Logger

	last map[string]reading // per run: the last raw reading and the total it gave
}

type reading struct{ raw, total int64 }

// NeedLeaderElection is true: two meters would each add the same increase.
func (m *Meter) NeedLeaderElection() bool { return true }

func (m *Meter) log() *slog.Logger {
	if m.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return m.Log
}

// total is a run's monotonic usage (R12). The gateway counter restarts when a data-plane pod does,
// so the raw value is never the total: the total is the high-water mark (the annotation, which
// SP1 never lets drop, or this meter's own last total) plus the increases since the last reading.
// A drop re-baselines and adds nothing, so a reset can hide at most one tick of usage.
func (m *Meter) total(r runs.Run, raw int64, seen bool) int64 {
	if m.last == nil {
		m.last = map[string]reading{}
	}
	prev, known := m.last[r.ID]
	hw := max(r.Tokens, prev.total)
	if !seen {
		return hw
	}
	t := hw
	switch {
	case !known: // a new run, or a new leader: the annotation is the floor
		t = max(hw, raw)
	case raw >= prev.raw:
		t = hw + (raw - prev.raw)
	}
	m.last[r.ID] = reading{raw: raw, total: t}
	return t
}

// forget drops the memory of runs that no longer exist.
func (m *Meter) forget(all []runs.Run) {
	live := make(map[string]bool, len(all))
	for _, r := range all {
		live[r.ID] = true
	}
	for id := range m.last {
		if !live[id] {
			delete(m.last, id)
		}
	}
}

// Start ticks at once, then every Every, until ctx ends. A failed tick is logged and the next
// one retries.
func (m *Meter) Start(ctx context.Context) error {
	if m.Every <= 0 {
		return fmt.Errorf("meter: the period %s is not positive", m.Every)
	}
	tick := m.Ticker
	if tick == nil {
		tick = func(d time.Duration) (<-chan time.Time, func()) { t := time.NewTicker(d); return t.C, t.Stop }
	}
	c, stop := tick(m.Every)
	defer stop()
	for {
		if err := m.Tick(ctx); err != nil {
			m.log().Warn("meter tick failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-c:
		}
	}
}

// Tick meters every run once: usage when it grew, budget-run for a live run at its cap. A failed
// read writes nothing; a failed annotation is reported and is no revocation.
func (m *Meter) Tick(ctx context.Context) error {
	all, err := m.Runs.List(ctx)
	if err != nil {
		return fmt.Errorf("meter: list runs: %w", err)
	}
	used, err := m.Source.RunTokens(ctx)
	if err != nil {
		return fmt.Errorf("meter: read usage: %w", err)
	}
	defer m.forget(all)
	var errs []error
	for _, r := range all {
		raw, seen := used[r.ID]
		n := m.total(r, raw, seen) // never the raw counter (R12)
		kv := map[string]string{}
		if n > r.Tokens {
			kv[runs.AnnUsage] = strconv.FormatInt(n, 10)
		}
		if !runs.Terminal(r.Phase) && r.Revoked == "" && r.MaxTokens > 0 && n >= r.MaxTokens {
			kv[runs.AnnRevoked] = ReasonBudgetRun
		}
		if len(kv) == 0 {
			continue
		}
		if err := m.Runs.Annotate(ctx, r.ID, kv); err != nil {
			errs = append(errs, fmt.Errorf("meter: run %s: %w", r.ID, err))
			continue
		}
		if why := kv[runs.AnnRevoked]; why != "" {
			m.log().Info("run revoked at its cap", "run", r.ID, "tokens", n, "maxTokens", r.MaxTokens, "reason", why)
			if m.OnRevoke != nil {
				m.OnRevoke(ctx, why)
			}
		}
	}
	return errors.Join(errs...)
}
