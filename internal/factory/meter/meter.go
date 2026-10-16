// SPDX-License-Identifier: Apache-2.0

// Package meter is the run meter (§4, C3, C5): every poll.meter, on every run, it writes the
// usage annotation the composition projects into status.usage.tokens, and revokes a live run at
// its cap: budget-run for the run's own (in USD when its tier sets runUSD, OD-6 (b), else in
// tokens), budget-fleet for a gateway 429 of the fleet bucket,
// budget-principal for the principal's day when budgets.enforcePrincipal (R13, C5). The day's
// spend is the R50 ledger's, appended here tick by tick (R07); Crossplane owns AgentRun status,
// so this writes annotations only. Every run is metered, human-launched ones included.
package meter

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/api"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/runs"
	"github.com/Smana/agent-platform/internal/factory/taskid"
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

// The limit behind a revocation. The reason stays C5's closed set, since the composition maps
// each value to a phase, so the limit goes to the log and the revocations metric instead.
const (
	LimitRunUSD         = "run-usd"
	LimitRunTokens      = "run-tokens"
	LimitGatewayCeiling = "gateway-ceiling" // B1
	LimitFleet          = "fleet"           // B2
	LimitPrincipalDay   = "principal-day"
)

// Why a run whose tier caps in USD is capped in tokens instead.
const (
	FallbackUnpriced = "unpriced" // its claim's model has no meter.prices entry
	FallbackUntyped  = "untyped"  // meter.query does not group by gen_ai_token_type
)

// ledgerRetention is how long a day's ledger survives: a month of budget plus a week's margin
// for an operator's audit. Older days are deleted by the meter, its only writer after them.
const ledgerRetention = 35 * 24 * time.Hour

// BlindTicks is how many periods without a metered tick admission tolerates before an enforced
// daily cap refuses (#54): a leader handover or a scrape gap is not an outage, five minutes at
// a 30 s period is.
const BlindTicks = 10

// orphanGrace is how long a reservation may stay without any claim before the meter releases
// it (#55). An admission reserves, then creates its claim within seconds; a reservation released
// in that gap is re-made by the factory's next Queued, and at worst leaves one run unreserved.
const orphanGrace = 2 * time.Minute

// Store is the part of the AgentRun client the meter uses; runs.Client implements it.
type Store interface {
	List(ctx context.Context) ([]runs.Run, error)
	Annotate(ctx context.Context, id string, kv map[string]string) error
}

// Usage is a run's raw counters. Tokens is what the token caps count: input plus output, or an
// untyped series' value. `Input` is the gateway's `input` series, which **includes** `cached_input`
// and `cache_creation_input`; `cost` subtracts them to price fresh input.
type Usage struct {
	Tokens, Input, CacheRead, CacheWrite, Output int64
	// Typed: the reading carried gen_ai_token_type, so it can be priced.
	Typed bool
	// Models splits the typed counters by the model that served them (gen_ai_request_model), so
	// each is priced at its own rate; "" holds a series without that label, priced by the claim's
	// model. nil on a typed reading: all of it is "".
	Models map[string]ModelUsage
}

// ModelUsage is one served model's typed counters within a run's Usage.
type ModelUsage struct {
	Input, CacheRead, CacheWrite, Output int64
}

// Source reads each run's raw counters, keyed by run id; VM implements it.
type Source interface {
	RunUsage(ctx context.Context) (map[string]Usage, error)
}

// Cost is what the meter knows of a run's USD cost: MicroUSD once a typed reading priced it,
// or Why it cannot be priced (FallbackUnpriced, FallbackUntyped).
type Cost struct {
	MicroUSD int64
	Why      string
}

// Costs hands each tick's run costs to the reconciler, which sums a task's for its USD cap
// (OD-6 (b)). Safe for concurrent use: the meter writes it, the reconciler reads it.
type Costs struct {
	mu sync.RWMutex
	m  map[string]Cost
}

// Get is a run's cost as of the meter's last tick; false when the meter has no word on it.
func (c *Costs) Get(id string) (Cost, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	v, ok := c.m[id]
	return v, ok
}

func (c *Costs) put(m map[string]Cost) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m = m
}

// cost prices a typed reading in micro-USD: tokens times USD per million is micro-USD.
func cost(p config.Price, u Usage) int64 {
	write := p.CacheWrite
	if write == 0 {
		write = p.Input
	}
	fresh := max(0, u.Input-u.CacheRead-u.CacheWrite)
	return int64(math.Round(float64(fresh)*p.Input + float64(u.CacheRead)*p.CacheRead +
		float64(u.CacheWrite)*write + float64(u.Output)*p.Output))
}

// runCost prices a typed reading in micro-USD as the sum over the models that served it, each at
// its own price; the claim's model prices the tokens no model label names. It returns the first
// served model with tokens and no price instead: such a run is unpriced, never $0.
func runCost(prices map[string]config.Price, claim string, u Usage) (micro int64, unpriced string) {
	by, unpriced := runCosts(prices, claim, u)
	for _, v := range by {
		micro += v
	}
	return micro, unpriced
}

// runCosts is runCost by the model that served the tokens, the claim's model for those no label
// names; nil and the model when one has no price.
func runCosts(prices map[string]config.Price, claim string, u Usage) (map[string]int64, string) {
	models := u.Models
	if len(models) == 0 {
		models = map[string]ModelUsage{"": {Input: u.Input, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite, Output: u.Output}}
	}
	out := map[string]int64{}
	for _, name := range slices.Sorted(maps.Keys(models)) {
		mu := models[name]
		if mu == (ModelUsage{}) {
			continue
		}
		if name == "" {
			name = claim
		}
		p, ok := prices[name]
		if !ok {
			return nil, name
		}
		out[name] += cost(p, Usage{Input: mu.Input, CacheRead: mu.CacheRead, CacheWrite: mu.CacheWrite, Output: mu.Output})
	}
	return out, ""
}

// endSettle is how long an ended run's cost waits before it is reported: a run's last calls reach
// the meter a scrape interval plus VictoriaMetrics' 30 s latency offset after they happened, so
// the first tick that sees a run ended may read it short (xplane-run-nh5tztlh: 370,526 of 507,350
// tokens). The meter keeps annotating an ended run whatever this window; only the report waits.
const endSettle = 2 * time.Minute

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
	// Prices are meter.prices, by the model that served the tokens (or the claim's logical model,
	// for tokens no model label names); RunUSD each tier's USD run cap (config.Tier.RunUSD). A run
	// of a tier with a cap is revoked on its USD cost, not its MaxTokens, when every model that
	// served it is priced and its reading typed; otherwise on MaxTokens, reported through
	// OnFallback.
	Prices map[string]config.Price
	RunUSD map[string]float64
	// Costs receives each tick's run costs, for the task cap; nil: nobody reads them.
	Costs *Costs
	Every time.Duration
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// OnRevoke is told of each revocation written, with its reason and the limit that fired:
	// fmetrics.Set.RunRevoked.
	OnRevoke func(ctx context.Context, reason, limit string)
	// OnCost is told, once, of a priced run's USD cost, in total and by the model that served it:
	// endSettle after the meter, having seen it live, sees it end, or when it is deleted first.
	// OnFallback of a run of a USD-capped tier capped in tokens instead, once per run.
	OnCost     func(ctx context.Context, tier string, usd float64, byModel map[string]float64)
	OnFallback func(ctx context.Context, why string)
	// OnLateCost is told of what a run spent after its cost was reported, by served model: a
	// reading slower than endSettle corrects the total spend, never the reported cost.
	OnLateCost func(ctx context.Context, byModel map[string]float64)
	// OnSpend receives, each tick that read usage, the USD spend of every priced run whose cost
	// is not reported yet, by run then served model: the per-run gauge.
	OnSpend func(spend map[string]map[string]float64)
	// OnUnpriced is told, once per run, of a model that served it with no price (J-AC4).
	OnUnpriced func(ctx context.Context, model string)
	// Remaining reports each principal's day budget after this tick: the fmetrics gauge.
	Remaining func(principal string, remaining, limit int64)
	// OnUsageError is told of each failed usage read; OnSucceeded of each tick that metered,
	// with its time; OnOvershoot of what a run spent past its cap when revoked (#54).
	OnUsageError func(ctx context.Context)
	OnSucceeded  func(ctx context.Context, at time.Time)
	OnOvershoot  func(ctx context.Context, tokens int64)
	// Ledger is the store of R50's day ledgers (the manager's client): the meter appends each
	// tick's increase to the spent column, releases the reservations of runs that ended, stamps
	// the heartbeat admission reads (#54), and prunes old days. nil: the day's spend lives in this meter's memory alone, and a failover loses it —
	// fine while EnforcePrincipal is off, which is every wiring that leaves it unset today.
	Ledger client.Client
	// Tasks lists the factory's Tasks (the manager's cached client: no apiserver call per tick),
	// so a live task's reserved next run is never released as unclaimed. nil: none is protected.
	Tasks     client.Reader
	Namespace string
	// Ticker starts the period; nil means a time.Ticker.
	Ticker func(d time.Duration) (c <-chan time.Time, stop func())
	Log    *slog.Logger

	last    map[string]reading          // per run: the last raw reading and the total it gave
	mem     map[string]int64            // the day's spend when no Ledger is wired
	memDay  string                      // the UTC day mem counts; a change resets it
	dropped map[string]bool             // ended runs whose reservation was dropped
	orphans map[string]time.Time        // unclaimed reservations, by when this meter first saw them
	pruned  string                      // the last UTC day whose old ledgers were deleted
	live    map[string]string           // priced runs seen live, by tier: their cost is still to report
	ended   map[string]time.Time        // live-seen runs, by when this meter first saw them ended
	settled map[string]map[string]int64 // reported runs: the micro-USD by served model reported
	fell    map[string]bool             // runs whose fallback was reported
	named   map[string]bool             // run and model pairs whose missing price was reported
}

type reading struct {
	raw, total int64
	paid       int64 // the total already appended to a day's ledger
	// The USD total in micro-dollars, kept like the tokens' but in memory alone: the factory's
	// patch limit admits no new annotation, so a failover after a counter reset forgets what
	// the run spent before that reset.
	usd      int64
	usdKnown bool
	models   map[string]mono // the USD by served model: usd is their sum
}

// mono is one monotonic counter's last raw value and the total it gave.
type mono struct{ raw, total int64 }

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
	prev.raw, prev.total = raw, t
	m.last[r.ID] = prev
	return t
}

// spend is a run's monotonic USD total from its raw cost by served model, as total is for
// tokens: each model's counter grows on its own, so a reset of one hides none of the others.
func (m *Meter) spend(id string, raw map[string]int64) int64 {
	if m.last == nil {
		m.last = map[string]reading{}
	}
	e := m.last[id]
	if e.models == nil {
		e.models = map[string]mono{}
	}
	e.usd = 0
	for model, v := range raw {
		p := e.models[model]
		switch {
		case !e.usdKnown:
			p.total = v
		case v >= p.raw:
			p.total += v - p.raw
		}
		p.raw = v
		e.models[model] = p
	}
	for _, p := range e.models {
		e.usd += p.total
	}
	e.usdKnown = true
	m.last[id] = e
	return e.usd
}

// usdCap is the run's cap in micro-USD, or 0 and why it is capped in tokens instead ("" for a
// tier with no USD cap). why is the run's pricing verdict this tick: "" when priced or unread.
func (m *Meter) usdCap(r runs.Run, why string) (int64, string) {
	c := m.RunUSD[r.Tier]
	switch {
	case c <= 0:
		return 0, ""
	case why != "":
		return 0, why
	}
	return int64(math.Round(c * 1e6)), ""
}

// forget drops the memory of runs that no longer exist, first reporting the cost of any deleted
// before its report.
func (m *Meter) forget(ctx context.Context, all []runs.Run) {
	live := make(map[string]bool, len(all))
	for _, r := range all {
		live[r.ID] = true
	}
	for id := range m.live {
		if !live[id] {
			m.report(ctx, id)
		}
	}
	for id := range m.last {
		if !live[id] {
			delete(m.last, id)
		}
	}
	for k := range m.named {
		if id, _, _ := strings.Cut(k, "/"); !live[id] {
			delete(m.named, k)
		}
	}
	for id := range m.settled {
		if !live[id] {
			delete(m.settled, id)
		}
	}
	for _, set := range []map[string]bool{m.dropped, m.fell} {
		for id := range set {
			if !live[id] {
				delete(set, id)
			}
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
// usage as it grew, revokes a live run at any of its caps, stamps the heartbeat admission reads,
// and keeps the ledgers themselves clean. A failed read of runs or usage writes no usage and no
// stamp. A ledger that cannot be written writes no usage annotation (a new leader takes the
// annotation as already paid), no stamp and no upkeep, but still revokes at the per-run caps. A
// failed annotation is reported and is no revocation. Occupancy is not usage: a tick that could
// not read usage still releases the runs that ended (#55), provided the ledger can be written.
func (m *Meter) Tick(ctx context.Context) error {
	all, err := m.Runs.List(ctx)
	if err != nil {
		return fmt.Errorf("meter: list runs: %w", err)
	}
	used, err := m.Source.RunUsage(ctx)
	if err != nil {
		if m.OnUsageError != nil {
			m.OnUsageError(ctx)
		}
		err = fmt.Errorf("meter: read usage: %w", err)
		if m.Ledger != nil {
			err = errors.Join(err, m.release(ctx, all))
		}
		m.forget(ctx, all)
		return err
	}
	var hit map[string]bool
	if m.Throttle != nil {
		if hit, err = m.Throttle.RecentlyThrottled(ctx); err != nil {
			hit = nil
			m.log().Warn("429 lookup failed; budget-fleet cannot be named this tick", "err", err)
		}
	}
	defer m.forget(ctx, all)
	day := m.now().UTC().Format(api.LedgerDayLayout)
	totals := make(map[string]int64, len(all))
	usd := map[string]int64{}
	caps := map[string]int64{}
	costs := map[string]Cost{}
	delta := map[string]int64{}
	pending := map[string]int64{}
	for _, r := range all {
		u, seen := used[r.ID]
		paid := r.Tokens // a meter with no memory starts paid at the annotation: its writer appended it
		if e, known := m.last[r.ID]; known {
			paid = e.paid
		}
		t := m.total(r, u.Tokens, seen) // never the raw counter (R12)
		totals[r.ID] = t
		var why string // the pricing verdict: "" priced, or unread
		switch {
		case !seen:
			if e := m.last[r.ID]; e.usdKnown {
				usd[r.ID], costs[r.ID] = e.usd, Cost{MicroUSD: e.usd}
			}
		case !u.Typed:
			why = FallbackUntyped
		default:
			raw, model := runCosts(m.Prices, r.Model, u)
			if model != "" {
				why = FallbackUnpriced
				m.unpriced(ctx, r, model)
				break
			}
			v := m.spend(r.ID, raw)
			usd[r.ID], costs[r.ID] = v, Cost{MicroUSD: v}
		}
		if why != "" {
			costs[r.ID] = Cost{Why: why}
		}
		if caps[r.ID], why = m.usdCap(r, why); why != "" {
			m.fallback(ctx, r, why)
		}
		if inc := t - paid; inc > 0 { // the increase since this run's last settled reading (R07)
			delta[r.Principal] += inc
			pending[r.ID] = t
		}
	}
	if m.Costs != nil {
		m.Costs.put(costs)
	}
	spent, settleErr := m.settle(ctx, day, delta) // budget-principal reads the ledger, not the live runs (R07)
	var errs []error
	if settleErr != nil {
		// A ledger that cannot be written delays the daily budget, never a run's own cap: the
		// loop below still revokes. Nothing is marked paid, so the increases are retried whole.
		errs = append(errs, settleErr)
		spent = m.readSpent(ctx, day) // read-only; nil when that fails too: budget-principal waits
	} else {
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
	}
	for _, r := range all {
		n := totals[r.ID]
		kv := map[string]string{}
		if n > r.Tokens && settleErr == nil { // an unpaid increase is never annotated: a new leader would skip it
			kv[runs.AnnUsage] = strconv.FormatInt(n, 10)
		}
		var limit string
		if !runs.Terminal(r.Phase) && r.Revoked == "" {
			if reason, l := m.revocation(r, n, usd[r.ID], caps[r.ID], hit[r.ID], spent[r.Principal]); reason != "" {
				kv[runs.AnnRevoked], limit = reason, l
			}
		}
		m.observeCost(ctx, r, usd)
		if len(kv) == 0 {
			continue
		}
		if err := m.Runs.Annotate(ctx, r.ID, kv); err != nil {
			errs = append(errs, fmt.Errorf("meter: run %s: %w", r.ID, err))
			continue
		}
		if why := kv[runs.AnnRevoked]; why != "" {
			m.log().Info("run revoked", "run", r.ID, "tokens", n, "usd", float64(usd[r.ID])/1e6,
				"cap_usd", float64(caps[r.ID])/1e6, "reason", why, "limit", limit)
			if m.OnRevoke != nil {
				m.OnRevoke(ctx, why, limit)
			}
			if over := n - r.MaxTokens; why == ReasonBudgetRun && limit != LimitRunUSD && r.MaxTokens > 0 && over > 0 && m.OnOvershoot != nil {
				m.OnOvershoot(ctx, over)
			}
		}
	}
	if m.OnSpend != nil {
		m.OnSpend(m.liveSpend())
	}
	// The success is reported only once admission can read it: the gauge never looks healthy
	// beside a 503 meter_blind.
	if settleErr != nil { // no stamp, no success, no ledger upkeep: the meter must not look healthy
		return errors.Join(errs...)
	}
	at := m.now()
	var stamp error
	if m.Ledger != nil {
		stamp = api.StampMeter(ctx, m.Ledger, m.Namespace, at, BlindTicks*m.Every)
		errs = append(errs, stamp, m.release(ctx, all), m.prune(ctx, day))
	}
	if stamp == nil && m.OnSucceeded != nil {
		m.OnSucceeded(ctx, at)
	}
	return errors.Join(errs...)
}

// readSpent reads the day's spent column without writing; nil when there is no Ledger, no day
// yet or the read fails (the caller then does not evaluate budget-principal).
func (m *Meter) readSpent(ctx context.Context, day string) map[string]int64 {
	if m.Ledger == nil {
		return nil
	}
	cm := &corev1.ConfigMap{}
	key := types.NamespacedName{Namespace: m.Namespace, Name: api.LedgerPrefix + day}
	if err := m.Ledger.Get(ctx, key, cm); err != nil {
		return nil
	}
	spent, err := api.LedgerSpentColumn(cm)
	if err != nil {
		return nil
	}
	return spent
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

// revocation names why a live run must stop, and the limit that fired, in the order the design
// lists the caps (C5). A run capped in USD is not held to its MaxTokens: pricing cache reads at
// their own rate is the point (OD-3), and B1 still bounds its raw tokens at the gateway.
func (m *Meter) revocation(r runs.Run, tokens, usd, capUSD int64, throttled bool, principalToday int64) (reason, limit string) {
	switch {
	case capUSD > 0 && usd >= capUSD:
		return ReasonBudgetRun, LimitRunUSD
	case capUSD <= 0 && r.MaxTokens > 0 && tokens >= r.MaxTokens:
		return ReasonBudgetRun, LimitRunTokens
	case throttled && m.B1Ceiling > 0 && tokens >= m.B1Ceiling:
		return ReasonBudgetRun, LimitGatewayCeiling
	case throttled:
		return ReasonBudgetFleet, LimitFleet // B2, the only other bucket on agent-router (R13)
	case m.Budgets.EnforcePrincipal && principalToday >= m.dailyCap(r.Principal):
		return ReasonBudgetPrincipal, LimitPrincipalDay
	}
	return "", ""
}

// fallback reports, once per live run, that its tier's USD cap could not apply. Never silent:
// an unpriced run metered at $0 would never stop.
func (m *Meter) fallback(ctx context.Context, r runs.Run, why string) {
	if runs.Terminal(r.Phase) || m.fell[r.ID] {
		return
	}
	if m.fell == nil {
		m.fell = map[string]bool{}
	}
	m.fell[r.ID] = true
	m.log().Warn("run capped in tokens: its tier's USD cap cannot be priced", "run", r.ID, "tier", r.Tier,
		"model", r.Model, "why", why, "max_tokens", r.MaxTokens)
	if m.OnFallback != nil {
		m.OnFallback(ctx, why)
	}
}

// unpriced names, once per run, a model that served it without a price: AgentModelUnpriced's
// signal, whatever the run's tier caps in.
func (m *Meter) unpriced(ctx context.Context, r runs.Run, model string) {
	k := r.ID + "/" + model
	if m.named[k] {
		return
	}
	if m.named == nil {
		m.named = map[string]bool{}
	}
	m.named[k] = true
	m.log().Warn("a model served a run without a price in meter.prices", "run", r.ID, "model", model)
	if m.OnUnpriced != nil {
		m.OnUnpriced(ctx, model)
	}
}

// observeCost reports a priced run's cost once, endSettle after the first tick that sees it
// ended following one that saw it live: a run that ended before this meter started is not
// counted again.
func (m *Meter) observeCost(ctx context.Context, r runs.Run, usd map[string]int64) {
	if _, priced := usd[r.ID]; !priced {
		return
	}
	if _, done := m.settled[r.ID]; done {
		m.lateCost(ctx, r.ID)
		return
	}
	if !runs.Terminal(r.Phase) {
		if m.live == nil {
			m.live = map[string]string{}
		}
		m.live[r.ID] = r.Tier
		return
	}
	if _, ok := m.live[r.ID]; !ok {
		return
	}
	if m.ended == nil {
		m.ended = map[string]time.Time{}
	}
	since, ok := m.ended[r.ID]
	if !ok {
		since = m.now()
		m.ended[r.ID] = since
	}
	if m.now().Sub(since) >= endSettle {
		m.report(ctx, r.ID)
	}
}

// report hands a live-seen run's cost to OnCost, once.
func (m *Meter) report(ctx context.Context, id string) {
	tier := m.live[id]
	delete(m.live, id)
	delete(m.ended, id)
	e := m.last[id]
	if !e.usdKnown {
		return
	}
	if m.settled == nil {
		m.settled = map[string]map[string]int64{}
	}
	at := make(map[string]int64, len(e.models))
	by := make(map[string]float64, len(e.models))
	for model, p := range e.models {
		at[model], by[model] = p.total, float64(p.total)/1e6
	}
	m.settled[id] = at
	if m.OnCost != nil {
		m.OnCost(ctx, tier, float64(e.usd)/1e6, by)
	}
}

// lateCost reports what a reported run spent since, by served model.
func (m *Meter) lateCost(ctx context.Context, id string) {
	at := m.settled[id]
	late := map[string]float64{}
	for model, p := range m.last[id].models {
		if d := p.total - at[model]; d > 0 {
			late[model], at[model] = float64(d)/1e6, p.total
		}
	}
	if len(late) == 0 {
		return
	}
	m.log().Warn("a run spent after its cost was reported: its readings trailed the settle window", "run", id, "usd_by_model", late)
	if m.OnLateCost != nil {
		m.OnLateCost(ctx, late)
	}
}

// liveSpend is the USD spend of each run whose cost is still to report, by served model.
func (m *Meter) liveSpend() map[string]map[string]float64 {
	out := make(map[string]map[string]float64, len(m.live))
	for id := range m.live {
		by := map[string]float64{}
		for model, p := range m.last[id].models {
			by[model] = float64(p.total) / 1e6
		}
		out[id] = by
	}
	return out
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

// release ends the admission reservations (R50) of the runs that no longer run, the API's and
// the factory's Queued's alike (#78, #55): a terminal run, a claim being deleted — a stop, the
// stuck and Pending bounds and the kill switch's sweep delete live claims, which never reach a
// terminal phase — and, after orphanGrace, a reservation no claim holds at all: its claim gone
// before any tick saw it end, or never created. The reconciler and the sweep release their own
// at once; this is the crash-safe floor under them. A failure is retried on the next tick.
func (m *Meter) release(ctx context.Context, all []runs.Run) error {
	return errors.Join(m.dropReservations(ctx, all), m.releaseOrphans(ctx, all))
}

// dropReservations releases an ended run's reservation. It sits in the ledger of the day that
// admitted the run — its creation day — so that day and today are searched (the two differ only
// for a run admitted as the clock crossed midnight).
func (m *Meter) dropReservations(ctx context.Context, all []runs.Run) error {
	now := m.now()
	var errs []error
	for _, r := range all {
		if ended := runs.Terminal(r.Phase) || r.Deleting; !ended || m.dropped[r.ID] {
			continue
		}
		err := api.Release(ctx, m.Ledger, m.Namespace, r.Created, r.ID)
		if !sameDay(r.Created, now) && err == nil {
			err = api.Release(ctx, m.Ledger, m.Namespace, now, r.ID)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("meter: run %s: %w", r.ID, err))
			continue // not dropped yet: retry next tick
		}
		if m.dropped == nil {
			m.dropped = map[string]bool{}
		}
		m.dropped[r.ID] = true
	}
	return errors.Join(errs...)
}

// releaseOrphans releases today's reservations that no listed claim has held for orphanGrace.
// Only today's ledger: admission reads no other. The grace lives in this meter's memory, so a
// new leader starts it again — later, never sooner.
func (m *Meter) releaseOrphans(ctx context.Context, all []runs.Run) error {
	now := m.now()
	key := types.NamespacedName{Namespace: m.Namespace, Name: api.LedgerPrefix + now.UTC().Format(api.LedgerDayLayout)}
	cm := &corev1.ConfigMap{}
	if err := m.Ledger.Get(ctx, key, cm); err != nil {
		if apierrors.IsNotFound(err) {
			m.orphans = nil
			return nil
		}
		return fmt.Errorf("meter: read the %s ledger: %w", key.Name, err)
	}
	claimed := make(map[string]bool, len(all))
	for _, r := range all {
		claimed[r.ID] = true
	}
	// A live task's next run may wait reserved and unclaimed for minutes in Queued (the room log,
	// the broker's permission), and its idempotent re-reserve refreshes nothing: releasing it
	// could start that run unreserved. The task's end releases it instead (#87 review).
	if m.Tasks != nil {
		var tasks v1alpha1.TaskList
		if err := m.Tasks.List(ctx, &tasks, client.InNamespace(m.Namespace)); err != nil {
			return fmt.Errorf("meter: list tasks: %w", err) // release nothing unclaimed on doubt
		}
		for i := range tasks.Items {
			if tk := &tasks.Items[i]; !v1alpha1.TerminalPhase(tk.Status.Phase) {
				claimed[taskid.Run(tk.Name, len(tk.Status.Runs))] = true
			}
		}
	}
	unclaimed := map[string]time.Time{}
	var due []string
	for k := range cm.Data {
		id, ok := strings.CutPrefix(k, api.LedgerReserved)
		if !ok || claimed[id] {
			continue
		}
		since, seen := m.orphans[id]
		if !seen {
			since = now
		}
		unclaimed[id] = since
		if now.Sub(since) >= orphanGrace {
			due = append(due, id)
		}
	}
	m.orphans = unclaimed
	if len(due) == 0 {
		return nil
	}
	if err := api.Release(ctx, m.Ledger, m.Namespace, now, due...); err != nil {
		return fmt.Errorf("meter: release unclaimed reservations: %w", err)
	}
	m.log().Info("unclaimed reservations released", "runs", due)
	for _, id := range due {
		delete(m.orphans, id)
	}
	return nil
}

func sameDay(a, b time.Time) bool {
	return a.UTC().Format(api.LedgerDayLayout) == b.UTC().Format(api.LedgerDayLayout)
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
