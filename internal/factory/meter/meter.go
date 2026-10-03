// SPDX-License-Identifier: Apache-2.0

// Package meter is the run meter (§4, C3, C5): every poll.meter, on every run, it writes the
// usage annotation the composition projects into status.usage.tokens, and revokes a live run at
// its cap: budget-run for the run's own, budget-fleet for a gateway 429 of the fleet bucket,
// budget-principal for the principal's day when budgets.enforcePrincipal (R13, C5). The day's
// spend is the R50 ledger's, appended here tick by tick (R07); Crossplane owns AgentRun status,
// so this writes annotations only. Every run is metered, human-launched ones included.
package meter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Smana/agent-platform/internal/factory/api"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/runs"
)

// Revocation reasons (C5). The composition turns each into its own BudgetExhausted variant,
// and the broker's end reason reads it back (runwatch.EndReason).
const (
	// ReasonBudgetRun is the revocation at a run's own cap (enforced from phase 1: R3).
	ReasonBudgetRun = "budget-run"
	// ReasonBudgetFleet is a gateway 429 below the run ceiling: the fleet bucket, the only
	// other one on agent-router (R13).
	ReasonBudgetFleet = "budget-fleet"
	// ReasonBudgetPrincipal is the principal's day spent (OD-10), read from the ledger (R07).
	ReasonBudgetPrincipal = "budget-principal"
)

// ledgerRetention is how long a day's ledger survives: a month of budget plus a week's margin
// for an operator's audit. Older days are deleted by the meter, its only writer after them.
const ledgerRetention = 35 * 24 * time.Hour

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
	// Throttle maps gateway 429s to runs; nil: no 429 mapping (VL implements it).
	Throttle Throttled
	// Budgets are the daily ceilings; enforced only when EnforcePrincipal (R3's shadow first).
	Budgets config.Budgets
	// B1Ceiling is the gateway's per-run ceiling (config.RunTokenCeiling): a 429 at or above it
	// is the run spending its own bucket, below it the fleet's (R13). Zero: never name B1.
	B1Ceiling int64
	Every     time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// OnRevoke is told of each revocation written, with its reason: fmetrics.Set.Revoked.
	OnRevoke func(ctx context.Context, reason string)
	// Remaining reports each principal's day budget after this tick: the fmetrics gauge.
	Remaining func(principal string, remaining, limit int64)
	// Ledger is the store of R50's day ledgers (the manager's client): the meter appends each
	// tick's increase to the spent column, drops a terminal run's reservation, and prunes old
	// days. nil: the day's spend lives in this meter's memory alone, and a failover loses it —
	// fine while EnforcePrincipal is off, which is every wiring that leaves it unset today.
	Ledger    client.Client
	Namespace string
	// Ticker starts the period; nil means a time.Ticker.
	Ticker func(d time.Duration) (c <-chan time.Time, stop func())
	Log    *slog.Logger

	last    map[string]reading // per run: the last raw reading and the total it gave
	mem     map[string]int64   // the day's spend when no Ledger is wired
	memDay  string             // the UTC day mem counts; a change resets it
	dropped map[string]bool    // terminal runs whose reservation was dropped
	pruned  string             // the last UTC day whose old ledgers were deleted
}

type reading struct {
	raw, total int64
	paid       int64 // the total already appended to a day's ledger
}

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
//
// A run first seen with no ledger memory starts paid at its annotation: the meter that wrote
// that annotation already appended that much to a day's ledger. A failover can therefore
// re-bill at most the increase of the annotation the previous leader failed to write, which
// over-counts — the safe direction for a budget.
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
		prev.paid = r.Tokens
	case raw >= prev.raw:
		t = hw + (raw - prev.raw)
	}
	m.last[r.ID] = reading{raw: raw, total: t, paid: prev.paid}
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
	for id := range m.dropped {
		if !live[id] {
			delete(m.dropped, id)
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

// Tick meters every run once: it appends the usage this tick saw to the day's ledger, writes
// usage as it grew, revokes a live run at any of its caps, and keeps the ledgers themselves
// clean. A failed read of runs, usage or the ledger writes nothing; a failed annotation is
// reported and is no revocation.
func (m *Meter) Tick(ctx context.Context) error {
	all, err := m.Runs.List(ctx)
	if err != nil {
		return fmt.Errorf("meter: list runs: %w", err)
	}
	used, err := m.Source.RunTokens(ctx)
	if err != nil {
		return fmt.Errorf("meter: read usage: %w", err)
	}
	var hit map[string]bool
	if m.Throttle != nil {
		if hit, err = m.Throttle.RecentlyThrottled(ctx); err != nil {
			hit = nil
			m.log().Warn("429 lookup failed; budget-fleet cannot be named this tick", "err", err)
		}
	}
	defer m.forget(all)
	day := m.now().UTC().Format(api.LedgerDayLayout)
	totals := make(map[string]int64, len(all))
	delta := map[string]int64{}
	pending := map[string]int64{}
	for _, r := range all {
		raw, seen := used[r.ID]
		paid := r.Tokens // a meter with no memory starts paid at the annotation: its writer appended it
		if e, known := m.last[r.ID]; known {
			paid = e.paid
		}
		t := m.total(r, raw, seen) // never the raw counter (R12)
		totals[r.ID] = t
		if inc := t - paid; inc > 0 { // the increase since this run's last settled reading (R07)
			delta[r.Principal] += inc
			pending[r.ID] = t
		}
	}
	spent, err := m.settle(ctx, day, delta) // budget-principal reads the ledger, not the live runs (R07)
	if err != nil {
		return err
	}
	for id, t := range pending { // the increase is in the day's ledger: those runs are paid up
		p := m.last[id]
		p.paid = t
		m.last[id] = p
	}
	if m.Remaining != nil {
		for p, n := range spent {
			m.Remaining(p, max(0, m.dailyCap(p)-n), m.dailyCap(p))
		}
	}
	var errs []error
	for _, r := range all {
		n := totals[r.ID]
		kv := map[string]string{}
		if n > r.Tokens {
			kv[runs.AnnUsage] = strconv.FormatInt(n, 10)
		}
		if !runs.Terminal(r.Phase) && r.Revoked == "" {
			if reason := m.revocation(r, n, hit[r.ID], spent[r.Principal]); reason != "" {
				kv[runs.AnnRevoked] = reason
			}
		}
		if len(kv) == 0 {
			continue
		}
		if err := m.Runs.Annotate(ctx, r.ID, kv); err != nil {
			errs = append(errs, fmt.Errorf("meter: run %s: %w", r.ID, err))
			continue
		}
		if why := kv[runs.AnnRevoked]; why != "" {
			m.log().Info("run revoked", "run", r.ID, "tokens", n, "reason", why)
			if m.OnRevoke != nil {
				m.OnRevoke(ctx, why)
			}
		}
	}
	if m.Ledger != nil {
		errs = append(errs, m.dropReservations(ctx, all), m.prune(ctx, day))
	}
	return errors.Join(errs...)
}

// settle appends this tick's increases to the day's ledger and returns the day's spend per
// principal (R07: the day an increase is observed is the day that carries it, not the day the
// run began). Admission reserves against the same column (R50), so the write retries conflicts
// like reserve does; a day that stays busy fails the tick, and the increases are retried whole
// on the next one. Without a Ledger the day lives in this meter's memory alone.
func (m *Meter) settle(ctx context.Context, day string, delta map[string]int64) (map[string]int64, error) {
	if m.Ledger == nil {
		if m.memDay != day { // the memory is one day wide: midnight starts it fresh
			m.mem, m.memDay = map[string]int64{}, day
		}
		for p, n := range delta {
			m.mem[p] += n
		}
		return m.mem, nil
	}
	key := types.NamespacedName{Namespace: m.Namespace, Name: api.LedgerPrefix + day}
	for range api.LedgerAttempts {
		cm := &corev1.ConfigMap{}
		err := m.Ledger.Get(ctx, key, cm)
		existed := true
		if apierrors.IsNotFound(err) {
			cm = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}}
			existed = false
		} else if err != nil {
			return nil, fmt.Errorf("meter: read the %s ledger: %w", day, err)
		}
		spent, err := api.LedgerSpentColumn(cm)
		if err != nil {
			return nil, fmt.Errorf("meter: the %s ledger: %w", day, err)
		}
		if len(delta) == 0 { // a quiet tick reads, never creates a ledger of nothing
			return spent, nil
		}
		if spent == nil {
			spent = map[string]int64{}
		}
		for p, n := range delta {
			spent[p] += n
		}
		if err := api.SetLedgerSpentColumn(cm, spent); err != nil {
			return nil, fmt.Errorf("meter: %w", err)
		}
		if existed {
			err = m.Ledger.Update(ctx, cm)
		} else {
			err = m.Ledger.Create(ctx, cm) // a concurrent Create conflicts; the loop re-reads
		}
		switch {
		case err == nil:
			return spent, nil
		case apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err):
			continue // re-read and re-append (R50)
		default:
			return nil, fmt.Errorf("meter: write the %s ledger: %w", day, err)
		}
	}
	return nil, fmt.Errorf("meter: the %s ledger stayed busy", day)
}

// revocation names why a live run must stop, in the order the design lists the caps (C5).
func (m *Meter) revocation(r runs.Run, tokens int64, throttled bool, principalToday int64) string {
	switch {
	case r.MaxTokens > 0 && tokens >= r.MaxTokens:
		return ReasonBudgetRun
	case throttled && m.B1Ceiling > 0 && tokens >= m.B1Ceiling:
		return ReasonBudgetRun // B1, the gateway's per-run ceiling
	case throttled:
		return ReasonBudgetFleet // B2, the only other bucket on agent-router (R13)
	case m.Budgets.EnforcePrincipal && principalToday >= m.dailyCap(r.Principal):
		return ReasonBudgetPrincipal
	}
	return ""
}

func (m *Meter) dailyCap(principal string) int64 {
	if principal == runs.PrincipalFactory {
		return m.Budgets.FactoryDaily
	}
	return m.Budgets.HumanDaily
}

func (m *Meter) now() time.Time {
	if m.Now == nil {
		return time.Now()
	}
	return m.Now()
}

// dropReservations releases a terminal run's admission reservation (R50): the meter is its only
// dropper once the API's own drop (create failure) no longer applies. The reservation sits in
// the ledger of the day that admitted the run — its creation day — so that day and today are
// searched (the two differ only for a run admitted as the clock crossed midnight). A run deleted
// before reaching terminal, or a day that stays busy, leaks at most one entry until its ledger
// is pruned: deleting a run refunds nothing (5.2). A failure is retried on the next tick, while
// the run is still listed.
func (m *Meter) dropReservations(ctx context.Context, all []runs.Run) error {
	today := m.now().UTC().Format(api.LedgerDayLayout)
	var errs []error
	for _, r := range all {
		if !runs.Terminal(r.Phase) || m.dropped[r.ID] {
			continue
		}
		created := r.Created.UTC().Format(api.LedgerDayLayout)
		err := m.dropReservation(ctx, created, r.ID)
		if created != today && err == nil {
			err = m.dropReservation(ctx, today, r.ID)
		}
		if err != nil {
			errs = append(errs, err)
			continue // not dropped yet: retry next tick
		}
		if m.dropped == nil {
			m.dropped = map[string]bool{}
		}
		m.dropped[r.ID] = true
	}
	return errors.Join(errs...)
}

func (m *Meter) dropReservation(ctx context.Context, day, id string) error {
	key := types.NamespacedName{Namespace: m.Namespace, Name: api.LedgerPrefix + day}
	ck := api.LedgerReserved + id
	for range api.LedgerAttempts {
		cm := &corev1.ConfigMap{}
		err := m.Ledger.Get(ctx, key, cm)
		if apierrors.IsNotFound(err) {
			return nil // no ledger that day: no reservation of this run there either
		}
		if err != nil {
			return fmt.Errorf("meter: read the %s ledger: %w", day, err)
		}
		if _, ok := cm.Data[ck]; !ok {
			return nil
		}
		delete(cm.Data, ck)
		err = m.Ledger.Update(ctx, cm)
		switch {
		case err == nil:
			return nil
		case apierrors.IsConflict(err):
			continue // an admission landed beside us: re-read and drop again
		default:
			return fmt.Errorf("meter: drop run %s reservation: %w", id, err)
		}
	}
	return fmt.Errorf("meter: the %s ledger stayed busy: run %s reservation kept", day, id)
}

// prune deletes the day ledgers older than ledgerRetention. Once per UTC day: a missed day of
// deletions is made up the next one, and the names are dated.
func (m *Meter) prune(ctx context.Context, day string) error {
	if m.pruned == day {
		return nil
	}
	var cms corev1.ConfigMapList
	if err := m.Ledger.List(ctx, &cms, client.InNamespace(m.Namespace)); err != nil {
		return fmt.Errorf("meter: list ledgers: %w", err)
	}
	cutoff := m.now().UTC().Add(-ledgerRetention).Format(api.LedgerDayLayout)
	var errs []error
	for i := range cms.Items {
		cm := &cms.Items[i]
		name, ok := strings.CutPrefix(cm.Name, api.LedgerPrefix)
		if !ok {
			continue
		}
		if _, err := time.Parse(api.LedgerDayLayout, name); err != nil {
			continue // a name that is not a day is not a ledger to us
		}
		if name >= cutoff { // the layout is ordered: so are the strings
			continue
		}
		if err := m.Ledger.Delete(ctx, cm); err != nil && !apierrors.IsNotFound(err) {
			errs = append(errs, fmt.Errorf("meter: prune %s: %w", cm.Name, err))
		}
	}
	if len(errs) == 0 {
		m.pruned = day // only a clean sweep stops the retries
	}
	return errors.Join(errs...)
}
