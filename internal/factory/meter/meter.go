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
	"cmp"
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

	v1alpha1 "github.com/Smana/maku/api/factory/v1alpha1"
	"github.com/Smana/maku/internal/factory/api"
	"github.com/Smana/maku/internal/factory/config"
	"github.com/Smana/maku/internal/factory/fmetrics"
	"github.com/Smana/maku/internal/factory/runs"
	"github.com/Smana/maku/internal/factory/taskid"
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

// fenceYesterday is how long past midnight a new leader's learn also seeds the day just ended,
// for a run it knows no entry on it, so that fence covers it: the run's first write may have been
// the lost one. What bounds it is how long after its send a write can still commit, the
// apiserver's request timeout (about 60 s), plus the lease handover (about 15 s) between the old
// leader's send and the new leader's learn; 10 minutes is a wide margin over both.
const fenceYesterday = 10 * time.Minute

// fenceAnnotation is what fence writes to a past day's ledger to move its resourceVersion on.
const fenceAnnotation = "agents.ogenki.io/fenced-at"

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
// untyped series' value. `Input` is the gateway's `input` series, which counts `cached_input` and
// `cache_creation_input` inside it on every path the platform runs: agentgateway 1.6's Chat
// Completions to Messages translation, streamed or not (measured 2026-10-22), and Z.ai's
// prompt_tokens. `cost` subtracts them to price fresh input, unless the input is below them
// (inputExcludesCache): only a gateway that leaves them out reports that.
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
	// Served is the models that served the run's typed reading, by gen_ai_request_model, sorted
	// (R10); nil when the query does not group by model.
	Served []string
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
	fresh := u.Input
	if !inputExcludesCache(u.Input, u.CacheRead, u.CacheWrite) {
		fresh -= u.CacheRead + u.CacheWrite
	}
	return int64(math.Round(float64(fresh)*p.Input + float64(u.CacheRead)*p.CacheRead +
		float64(u.CacheWrite)*write + float64(u.Output)*p.Output))
}

// inputExcludesCache is true of an input below its cache tokens: only a gateway that reports
// input without them yields one. An input at or above them is read as including them, the shape
// the internal runs' route reports; a run whose calls mix both shapes is priced as the larger.
func inputExcludesCache(input, read, write int64) bool { return input < read+write }

// served is the models with tokens in a typed reading, sorted; "" (no model label) is none.
func (u Usage) served() []string {
	var out []string
	for name, mu := range u.Models {
		if name != "" && mu != (ModelUsage{}) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// byModel is the typed counters by served model, "" holding a reading without the model label.
func (u Usage) byModel() map[string]ModelUsage {
	if len(u.Models) == 0 {
		return map[string]ModelUsage{"": {Input: u.Input, CacheRead: u.CacheRead, CacheWrite: u.CacheWrite, Output: u.Output}}
	}
	return u.Models
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
	models := u.byModel()
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
	// B1Ceiling is the gateway's per-run ceiling (config.RunTokenCeiling), in the unit B1 charges
	// (b1Spend): a 429 at or above it is the run spending its own bucket, below it the fleet's
	// (R13). Zero: never name B1.
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
	// OnRevoke is told of each revocation written, with its reason and the limit that fired, under
	// a context that binds the run's payer (fmetrics.WithPrincipal): fmetrics.Set.RunRevoked.
	OnRevoke func(ctx context.Context, reason, limit string)
	// OnCharge is told of each run's charge once the write that settles it succeeded, or once
	// resolve finds a write in doubt landed: its payer (runs.Run.Payer), its kind and the
	// micro-USD (spend per identity SI2). Its sum over payers is what the day's spent columns
	// gained: R10's lift reaches a run's settled entry, never spent, and is never counted.
	OnCharge func(ctx context.Context, principal, kind string, microUSD int64)
	// OnCharges receives, each tick that listed the runs, those the charge info names (SI3):
	// every run not ended, and each ended one until endSettle after its status.finishedAt. Each
	// hand-over is a slice of its own, kept by the receiver; Start hands over nil as it returns,
	// so a meter that stops leading names no run.
	OnCharges func(live []runs.Run)
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
	// OnSettled receives, each tick that listed the runs, what the day ledgers settled for each
	// run that has an entry, in USD: the sum of its settled.<runId> over the days, as the ledger
	// objects read or written show it (XC5), never the meter's own account of it. A run stays in it
	// settledKept after it ends, then leaves.
	OnSettled func(settled map[string]float64)
	// OnUnpriced is told, once per run, of a model that served it with no price (J-AC4).
	OnUnpriced func(ctx context.Context, model string)
	// OnInputExcludesCache is told, once per run, of a model whose input read below its cache
	// tokens: the gateway reported it without them, and it was priced as fresh input whole.
	OnInputExcludesCache func(ctx context.Context, model string)
	// Day reports each principal's day after a tick that settled, in micro-USD: what it spent,
	// unclamped, and its cap. The fmetrics gauges.
	Day func(principal string, spent, limit int64)
	// OnUsageError is told of each failed usage read; OnSucceeded of each tick that metered,
	// with its time; OnOvershoot of what a run spent past its cap when revoked (#54).
	OnUsageError func(ctx context.Context)
	OnSucceeded  func(ctx context.Context, at time.Time)
	OnOvershoot  func(ctx context.Context, tokens int64)
	// Ledger is the store of R50's day ledgers (the manager's client): the meter charges each
	// tick's increase, in micro-USD, to the spent column and to each run's settled entry in one
	// write, releases the reservations of runs that ended, stamps the heartbeat admission reads
	// (#54), and prunes old days. nil: the day's spend lives in this meter's memory alone, and a
	// failover loses it — fine while EnforcePrincipal is off, which is every wiring that leaves
	// it unset today.
	Ledger client.Client
	// Tasks lists the factory's Tasks (the manager's cached client: no apiserver call per tick),
	// so a live task's reserved next run is never released as unclaimed. nil: none is protected.
	Tasks     client.Reader
	Namespace string
	// Ticker starts the period; nil means a time.Ticker.
	Ticker func(d time.Duration) (c <-chan time.Time, stop func())
	Log    *slog.Logger

	last     map[string]reading          // per run: the last raw reading and the total it gave
	accounts map[string]account          // per run: its total as the day ledgers count it, and what they settled
	mem      map[string]int64            // the day's spend when no Ledger is wired
	memDay   string                      // the UTC day mem counts; a change resets it
	dropped  map[string]bool             // ended runs whose reservation was dropped
	orphans  map[string]time.Time        // unclaimed reservations, by when this meter first saw them
	pruned   string                      // the last UTC day whose old ledgers were deleted
	told     map[string]bool             // the principals Day was told of on toldDay
	toldDay  string                      // the UTC day told counts
	live     map[string]string           // priced runs seen live, by tier: their cost is still to report
	ended    map[string]time.Time        // live-seen runs, by when this meter first saw them ended
	settled  map[string]map[string]int64 // reported runs: the micro-USD by served model reported
	fell     map[string]bool             // runs whose fallback was reported
	ledger   map[string]*runLedger       // per run: its settled entries as the ledgers last showed them
	named    map[string]bool             // run and model pairs whose missing price, or input without cache, was reported
}

type reading struct {
	raw, total int64
	// The USD total in micro-dollars, kept like the tokens' and lifted, when this meter first
	// measures the run, to what the day ledgers settled for it (account.base): a failover after a
	// counter reset forgets nothing the run spent before that reset.
	usd      int64
	usdKnown bool
	models   map[string]mono // the USD by served model: usd is their sum plus the account's base
}

// account is a run as the day ledgers count it (truthful costs TC6): total is its priced cost, or
// its tokens at the reference rate when it cannot be priced, lifted by base to what earlier
// meters settled; settled is what the ledgers charged it as this meter knows it: every retained
// day as read plus what it wrote since. lift is R10's: what an ended run is taken as charged
// where no ledger shows it. It is not in settled: it stays pending and is written into the run's
// settled entry with its next charge (never into spent), so a new leader reading that entry sees
// the lift and does not charge the run's pre-switch cost again.
type account struct {
	total, settled, base int64
	known                bool             // settled was read from the ledgers
	measured             bool             // base is fixed: the run was measured once since
	doubt                string           // the day of a charge whose write reported an error: it may have landed
	was                  int64            // the run's settled entry on that day before the write
	lift                 int64            // R10's lift, recorded in the settled entry with the run's next charge
	seenDay              string           // the day of a write resolve judged lost: it may still commit (#166)
	seen                 int64            // the run's settled entry on seenDay when resolve read it
	learned              map[string]int64 // the run's settled entry by day as learn read it: what a lost write of a previous leader is netted against
	guarded              bool             // a write of this meter's own carries the run: nothing earlier can commit after it
	payer, kind          string           // whom and what its charges count for (runs.Run.Payer, Kind)
}

// settledKept is how long OnSettled keeps a run after it ends: longer than the cross-check's
// alert waits.
const settledKept = time.Hour

// runLedger is a run's settled entries by UTC day, as the ledger objects last showed them, and
// when it was first seen ended (zero while it runs).
type runLedger struct {
	days  map[string]int64
	ended time.Time
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
	prev.raw, prev.total = raw, t
	m.last[r.ID] = prev
	return t
}

// spend is a run's monotonic priced cost from its raw cost by served model, as total is for
// tokens: each model's counter grows on its own, so a reset of one hides none of the others. It
// returns the models' sum; measure lifts it to the run's USD total.
func (m *Meter) spend(id string, raw map[string]int64) int64 {
	if m.last == nil {
		m.last = map[string]reading{}
	}
	e := m.last[id]
	if e.models == nil {
		e.models = map[string]mono{}
	}
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
	var sum int64
	for _, p := range e.models {
		sum += p.total
	}
	e.usdKnown = true
	m.last[id] = e
	return sum
}

// learn reads what the day ledgers settled for each listed run this meter holds no account of:
// a new run, or every run after a failover. One list of the ledgers per tick that meets one;
// without a Ledger nothing was settled anywhere else. An unread ledger leaves those runs
// unaccounted, so none of them is charged until a later tick reads it.
func (m *Meter) learn(ctx context.Context, all []runs.Run) error {
	var need []string
	for _, r := range all {
		if !m.accounts[r.ID].known {
			need = append(need, r.ID)
		}
	}
	if len(need) == 0 {
		return nil
	}
	var settled map[string]int64
	var days map[string]map[string]int64
	if m.Ledger != nil {
		var err error
		days, err = api.SettledByRunDay(ctx, m.Ledger, m.Namespace)
		if err != nil {
			return fmt.Errorf("meter: %w", err)
		}
		settled = make(map[string]int64, len(days))
		for _, id := range need {
			for day, n := range days[id] {
				settled[id] += n
				m.note(day, id, n)
			}
		}
	}
	if m.accounts == nil {
		m.accounts = map[string]account{}
	}
	now := m.now().UTC()
	today, yesterday := now.Format(api.LedgerDayLayout), now.Add(-24*time.Hour).Format(api.LedgerDayLayout)
	recent := now.Sub(now.Truncate(24*time.Hour)) < fenceYesterday
	for _, id := range need {
		a := m.accounts[id]
		a.settled, a.known = settled[id], true
		// An earlier leader's lost write may still commit (#166): the days it can land on are the
		// run's entries as read, today's, and the day just ended while the window lasts. They are
		// seeded here, at learn's read, so fence covers them whenever it runs.
		a.learned, a.guarded = maps.Clone(days[id]), false
		if a.learned == nil {
			a.learned = map[string]int64{}
		}
		a.learned[today] = days[id][today]
		if recent {
			a.learned[yesterday] = days[id][yesterday]
		}
		m.accounts[id] = a
	}
	return nil
}

// resolve settles each run whose last charge's write reported an error as the ledger of that
// write's day shows it: what the run's entry there gained over was is what the write landed. A
// landed lift is in that entry, so it is cleared here. The rest of what the run settled stays as
// this meter knows it: the part no ledger shows, and days pruned since they were read. One read
// per day in doubt; a failed one fails the tick, as learn's does, and leaves the runs in doubt.
func (m *Meter) resolve(ctx context.Context) error {
	read := map[string]map[string]int64{}
	for id, a := range m.accounts {
		if a.doubt == "" {
			continue
		}
		entries, ok := read[a.doubt]
		if !ok {
			cm := &corev1.ConfigMap{}
			switch err := m.Ledger.Get(ctx, types.NamespacedName{Namespace: m.Namespace, Name: api.LedgerPrefix + a.doubt}, cm); {
			case apierrors.IsNotFound(err):
				cm = &corev1.ConfigMap{} // the write that would have created it never landed
			case err != nil:
				return fmt.Errorf("meter: read the %s ledger: %w", a.doubt, err)
			}
			var err error
			if entries, err = api.SettledColumn(cm); err != nil {
				return fmt.Errorf("meter: %w", err)
			}
			read[a.doubt] = entries
		}
		m.noteOf(a.doubt, entries, id)
		gain := max(0, entries[id]-a.was)
		if gain == 0 { // lost, as far as this read shows: settle watches for it to commit late
			a.seenDay, a.seen = a.doubt, entries[id]
		}
		a.settled += gain
		charge := gain
		if gain > 0 { // the write landed whole: the run's charge, and its lift, which spent never counts
			charge = max(0, gain-a.lift)
			a.lift = 0
		}
		a.doubt, a.was = "", 0
		m.accounts[id] = a
		m.charge(ctx, a, charge)
	}
	return nil
}

// measure takes a seen run's cost this tick, v, priced or at the reference rate, and returns its
// total as the day ledgers count it. The first measure once the run's account is read fixes base,
// which lifts v to what earlier meters settled: the larger of the two, as total starts tokens at
// the annotation (TC6). A run first seen ended and annotated is charged nothing more when nothing
// is settled for it, since the token ledgers charged it before the switch to USD, or when it began
// before the oldest retained ledger, since a pruned day holds some of its charge (TC9). A priced v
// is also the run's USD total, for its cap and its cost.
func (m *Meter) measure(r runs.Run, v int64, priced bool) int64 {
	a := m.accounts[r.ID]
	if a.known && !a.measured {
		a.base, a.measured = max(0, a.settled-v), true
		if runs.Terminal(r.Phase) && r.Tokens > 0 &&
			(a.settled == 0 || r.Created.UTC().Format(api.LedgerDayLayout) < m.retainedFrom()) {
			a.lift = max(0, v-a.settled)
		}
	}
	a.total = a.base + v
	if m.accounts == nil {
		m.accounts = map[string]account{}
	}
	m.accounts[r.ID] = a
	if priced {
		e := m.last[r.ID]
		e.usd = a.total
		m.last[r.ID] = e
	}
	return a.total
}

// unsettled is what the day ledgers have yet to charge for the run: its total less what they
// settled and less the lift still pending. Nothing until its account is read and the run measured once.
func (m *Meter) unsettled(id string) int64 {
	a := m.accounts[id]
	if !a.known || !a.measured {
		return 0
	}
	return max(0, a.total-a.settled-a.lift)
}

// usdCap is the run's cap in micro-USD, or 0 and why it is capped in tokens instead ("" for a
// tier with no USD cap). why is the run's pricing verdict this tick: "" when priced or unread.
func (m *Meter) usdCap(r runs.Run, why string) (int64, string) {
	tier := int64(math.Round(m.RunUSD[r.Tier] * 1e6))
	// An api task's run carries its own cap, a third of its ceiling (F4, C-1): the lower holds.
	c := r.MaxMicroUSD
	if tier > 0 && (c <= 0 || tier < c) {
		c = tier
	}
	switch {
	case c <= 0:
		return 0, ""
	case why != "":
		return 0, why
	}
	return c, ""
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
	for id, a := range m.accounts {
		if !live[id] && a.doubt == "" { // a write in doubt is resolved first: it may have landed
			delete(m.accounts, id)
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
	if m.OnCharges != nil { // the charge info is the leader's (SI3): it ends with the leadership
		defer m.OnCharges(nil)
	}
	if m.OnSettled != nil { // so is the settled gauge
		defer m.OnSettled(nil)
	}
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

// Tick meters every run once: it charges the cost this tick saw to the day's ledger, writes
// usage as it grew, revokes a live run at any of its caps, stamps the heartbeat admission reads,
// and keeps the ledgers themselves clean. A failed read of runs or usage writes no usage and no
// stamp. A ledger that cannot be read or written writes no usage annotation (a run's usage shows
// only what a day ledger charged), no stamp and no upkeep, but still revokes at the per-run caps.
// A failed annotation is reported and is no revocation. Occupancy is not usage: a tick that could
// not read usage still releases the runs that ended (#55), provided the ledger can be written.
func (m *Meter) Tick(ctx context.Context) error {
	all, err := m.Runs.List(ctx)
	if err != nil {
		return fmt.Errorf("meter: list runs: %w", err)
	}
	if m.OnCharges != nil {
		m.OnCharges(m.chargeInfo(all))
	}
	defer m.publishSettled(all)
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
	// What a failed write landed, and what earlier meters settled, before any run is charged.
	learnErr := errors.Join(m.resolve(ctx), m.learn(ctx, all))
	totals := make(map[string]int64, len(all))
	usd := map[string]int64{}
	caps := map[string]int64{}
	costs := map[string]Cost{}
	delta := map[string]int64{}   // this tick's charges by ledger key
	inc := map[string]int64{}     // and by run: its settled entry
	keys := map[string][]string{} // the ledger keys each charged run is charged to
	for _, r := range all {
		u, seen := used[r.ID]
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
			for name, mu := range u.byModel() {
				if inputExcludesCache(mu.Input, mu.CacheRead, mu.CacheWrite) {
					m.excludesCache(ctx, r, cmp.Or(name, r.Model))
				}
			}
			v := m.measure(r, m.spend(r.ID, raw), true)
			usd[r.ID], costs[r.ID] = v, Cost{MicroUSD: v}
		}
		if why != "" {
			costs[r.ID] = Cost{Why: why}
			m.measure(r, config.RefMicroUSD(t), false) // never $0: the reference rate (TC7)
		}
		if seen && u.Typed {
			c := costs[r.ID]
			c.Served = u.served()
			costs[r.ID] = c
		}
		if caps[r.ID], why = m.usdCap(r, why); why != "" {
			m.fallback(ctx, r, why)
		}
		if n := m.unsettled(r.ID); n > 0 { // charged to the day it is observed in (R07)
			keys[r.ID] = charged(r)
			for _, k := range keys[r.ID] {
				delta[k] += n
			}
			inc[r.ID] = n + m.accounts[r.ID].lift
			a := m.accounts[r.ID]
			a.payer, a.kind = r.Payer(), r.Kind()
			m.accounts[r.ID] = a
		}
	}
	if m.Costs != nil {
		m.Costs.put(costs)
	}
	var spent map[string]int64
	settleErr := learnErr
	if settleErr == nil {
		spent, settleErr = m.settle(ctx, day, delta, inc, keys) // budget-principal reads the ledger, not the live runs (R07)
	}
	var errs []error
	if settleErr != nil {
		// A ledger that cannot be read or written delays the daily budget, never a run's own cap:
		// the loop below still revokes. Nothing is settled, so the charges are retried whole.
		errs = append(errs, settleErr)
		spent = m.readSpent(ctx, day) // read-only; nil when that fails too: budget-principal waits
	} else {
		for id, n := range inc { // in the day's ledger: those runs are settled up
			a := m.accounts[id]
			charge := n - a.lift // the lift reaches the run's settled entry, never spent
			a.settled += n
			a.lift = 0
			m.accounts[id] = a
			m.charge(ctx, a, charge)
		}
		if m.Day != nil {
			m.tellDay(day, spent)
		}
	}
	for _, r := range all {
		n := totals[r.ID]
		kv := map[string]string{}
		// Usage shows only what a day ledger charged: an annotated run with nothing settled is one
		// the token ledgers charged before the switch (measure).
		if n > r.Tokens && settleErr == nil {
			kv[runs.AnnUsage] = strconv.FormatInt(n, 10)
		}
		var limit string
		if !runs.Terminal(r.Phase) && r.Revoked == "" {
			if reason, l := m.revocation(r, n, b1Spend(n, usd, r.ID), usd[r.ID], caps[r.ID], hit[r.ID],
				m.daySpent(r, spent)); reason != "" {
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
				m.OnRevoke(fmetrics.WithPrincipal(ctx, r.Payer()), why, limit)
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

// settle adds this tick's charges to the day's ledger, delta to the spent column by key and inc
// to each run's settled entry in the same write (TC6), and returns the day's spend per principal
// (R07: the day a charge is observed is the day that carries it, not the day the run began).
// Admission reserves against the same column (R50), so the write retries conflicts like reserve
// does; a day that stays busy fails the tick, and the charges are retried whole on the next one.
// keys names the ledger keys each run in inc is charged to.
// A write that reports any other error may have landed all the same (a timeout after the commit):
// settle leaves each charged run in doubt, with its entry before the write, for resolve. Without a
// Ledger the day lives in this meter's memory alone.
//
// A write resolve judged lost can still commit later (#166): its resourceVersion is the one the
// run's entry was read at (account.seen). Each attempt reads the entry again, and what it gained
// over seen is that write landing: it is taken off what this write adds, so the charge is added
// once. The write's own Update carries the resourceVersion of the read, so a late write that
// commits after it conflicts and never lands. A write that failed on another day than this one is
// fenced first (fence): no Update of that day's ledger from before the fence can land after it.
func (m *Meter) settle(ctx context.Context, day string, delta, inc map[string]int64, keys map[string][]string) (map[string]int64, error) {
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
	fenced, err := m.fence(ctx, day, inc)
	if err != nil {
		return nil, err
	}
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
		was, err := api.SettledColumn(cm)
		if err != nil {
			return nil, fmt.Errorf("meter: the %s ledger: %w", day, err)
		}
		for id := range inc {
			m.noteOf(day, was, id)
		}
		late := maps.Clone(fenced)
		for id := range inc {
			if base, ok := m.baseline(m.accounts[id], day); ok {
				late[id] += max(0, was[id]-base)
			}
		}
		add, addInc := m.net(delta, inc, keys, late)
		if len(add) == 0 && len(addInc) == 0 { // a quiet tick reads, never creates a ledger of nothing
			m.settledUp(inc)
			return spent, nil
		}
		if spent == nil {
			spent = map[string]int64{}
		}
		for p, n := range add {
			spent[p] += n
		}
		if err := api.SetLedgerSpentColumn(cm, spent); err != nil {
			return nil, fmt.Errorf("meter: %w", err)
		}
		if err := api.AddSettled(cm, addInc); err != nil {
			return nil, fmt.Errorf("meter: %w", err)
		}
		if existed {
			err = m.Ledger.Update(ctx, cm)
		} else {
			err = m.Ledger.Create(ctx, cm) // a concurrent Create conflicts; the loop re-reads
		}
		switch {
		case err == nil:
			if after, err := api.SettledColumn(cm); err != nil { // the object the write returned
				m.log().Warn("meter: the written ledger does not parse, the settled gauge keeps its last reading", "day", day, "err", err)
			} else {
				for id := range inc {
					m.noteOf(day, after, id)
				}
			}
			m.settledUp(inc)
			return spent, nil
		case apierrors.IsConflict(err) || apierrors.IsAlreadyExists(err):
			continue // re-read and re-append (R50)
		default:
			for id := range inc {
				a := m.accounts[id]
				if g := late[id]; g > 0 { // landed late: counted now, as resolve counts a landed write
					a.settled += g
					m.charge(ctx, a, max(0, g-a.lift))
					a.lift = 0
				}
				a.doubt, a.was = day, was[id]
				a.seenDay, a.seen = "", 0
				a.learned, a.guarded = nil, true
				m.accounts[id] = a
			}
			return nil, fmt.Errorf("meter: write the %s ledger: %w", day, err)
		}
	}
	return nil, fmt.Errorf("meter: the %s ledger stayed busy", day)
}

// note records a run's settled entry n on a day as a ledger object showed it; a run with no
// entry there has nothing to record.
func (m *Meter) note(day, id string, n int64) {
	if m.ledger == nil {
		m.ledger = map[string]*runLedger{}
	}
	l := m.ledger[id]
	if l == nil {
		l = &runLedger{days: map[string]int64{}}
		m.ledger[id] = l
	}
	l.days[day] = n
}

// noteOf records run id's entry of a ledger's settled column on day, when it has one.
func (m *Meter) noteOf(day string, entries map[string]int64, id string) {
	if n, ok := entries[id]; ok {
		m.note(day, id, n)
	}
}

// publishSettled hands OnSettled each run's ledger total, and lets go of the runs that ended
// settledKept ago: a run absent from the list counts as ended when first missed.
func (m *Meter) publishSettled(all []runs.Run) {
	if m.OnSettled == nil {
		return
	}
	now := m.now()
	listed := make(map[string]runs.Run, len(all))
	for _, r := range all {
		listed[r.ID] = r
	}
	out := make(map[string]float64, len(m.ledger))
	for id, l := range m.ledger {
		r, ok := listed[id]
		switch {
		case ok && !runs.Terminal(r.Phase):
			l.ended = time.Time{}
		case ok && !r.Finished.IsZero():
			l.ended = r.Finished
		case l.ended.IsZero():
			l.ended = now
		}
		if !l.ended.IsZero() && now.Sub(l.ended) >= settledKept {
			delete(m.ledger, id)
			continue
		}
		var sum int64
		for _, n := range l.days {
			sum += n
		}
		out[id] = float64(sum) / 1e6
	}
	m.OnSettled(out)
}

// settledUp ends the watch for a late commit of each run charged: the write that settled it
// carried a resourceVersion no earlier write can commit after.
func (m *Meter) settledUp(inc map[string]int64) {
	for id := range inc {
		a := m.accounts[id]
		a.seenDay, a.seen = "", 0
		a.learned, a.guarded = nil, true
		m.accounts[id] = a
	}
}

// baseline is the entry of the run's ledger on a day that a write lost to an earlier meter, or
// to this one, is netted against: what resolve read when it judged this meter's own write lost,
// else what learn read for a run no write of this meter carries yet (a new leader: the old one's
// 504 may still commit). Not ok when nothing can still commit there.
func (m *Meter) baseline(a account, day string) (int64, bool) {
	switch {
	case a.seenDay != "":
		return a.seen, a.seenDay == day
	case !a.guarded:
		return a.learned[day], true
	}
	return 0, false
}

// net is what this write adds once late, the micro-USD of each run that an earlier write
// committed meanwhile, is off: the spent column by key and the settled entries by run. What such
// a write settled includes the run's lift, which spent never counts.
func (m *Meter) net(delta, inc map[string]int64, keys map[string][]string, late map[string]int64) (map[string]int64, map[string]int64) {
	add, addInc := maps.Clone(delta), map[string]int64{}
	for id, n := range inc {
		g := late[id]
		if g > 0 {
			cut := max(0, g-m.accounts[id].lift)
			for _, k := range keys[id] {
				if cut > add[k] {
					m.log().Warn("meter: a late write landed more than this tick charges, the excess is not netted",
						"run", id, "key", k, "late", cut, "charge", add[k])
				}
				add[k] = max(0, add[k]-cut)
			}
		}
		if n-g > 0 {
			addInc[id] = n - g
		} else if g > n {
			m.log().Warn("meter: a late write landed more than this tick settles, the excess is not netted",
				"run", id, "late", g, "settles", n)
		}
	}
	for k, n := range add {
		if n == 0 {
			delete(add, k)
		}
	}
	return add, addInc
}

// fence settles the question of a lost write on a day other than today's: for each run in inc
// whose lost write was on one, it updates that day's ledger, so a write carrying an older
// resourceVersion conflicts from then on, and returns what the run's entry there gained over
// seen: the late write, if it committed before the fence. A day already pruned is let go.
func (m *Meter) fence(ctx context.Context, day string, inc map[string]int64) (map[string]int64, error) {
	days := map[string]bool{}
	for id := range inc {
		a := m.accounts[id]
		switch {
		case a.seenDay != "":
			if a.seenDay != day {
				days[a.seenDay] = true
			}
		case !a.guarded: // a new leader: the days learn read the run on, seeded at its read
			for d := range a.learned {
				days[d] = true
			}
			delete(days, day)
		}
	}
	late := map[string]int64{}
	for d := range days {
		key := types.NamespacedName{Namespace: m.Namespace, Name: api.LedgerPrefix + d}
		var entries map[string]int64
		for attempt := range api.LedgerAttempts {
			cm := &corev1.ConfigMap{}
			existed := true
			if err := m.Ledger.Get(ctx, key, cm); apierrors.IsNotFound(err) {
				if d < m.retainedFrom() {
					entries = nil
					break
				}
				cm, existed = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}}, false
			} else if err != nil {
				return nil, fmt.Errorf("meter: read the %s ledger: %w", d, err)
			}
			var err error
			if entries, err = api.SettledColumn(cm); err != nil {
				return nil, fmt.Errorf("meter: the %s ledger: %w", d, err)
			}
			if cm.Annotations == nil {
				cm.Annotations = map[string]string{}
			}
			cm.Annotations[fenceAnnotation] = m.now().UTC().Format(time.RFC3339Nano)
			if existed {
				err = m.Ledger.Update(ctx, cm)
			} else {
				err = m.Ledger.Create(ctx, cm)
			}
			if err == nil {
				break
			}
			if !apierrors.IsConflict(err) && !apierrors.IsAlreadyExists(err) {
				return nil, fmt.Errorf("meter: fence the %s ledger: %w", d, err)
			}
			if attempt == api.LedgerAttempts-1 {
				return nil, fmt.Errorf("meter: the %s ledger stayed busy", d)
			}
		}
		for id := range inc {
			if base, ok := m.baseline(m.accounts[id], d); ok {
				late[id] += max(0, entries[id]-base)
				m.noteOf(d, entries, id)
			}
		}
	}
	return late, nil
}

// charge tells OnCharge of n micro-USD the day's spent column now holds for a run, under its payer.
func (m *Meter) charge(ctx context.Context, a account, n int64) {
	if n > 0 && m.OnCharge != nil {
		m.OnCharge(ctx, a.payer, a.kind, n)
	}
}

// chargeInfo is the runs the charge info names (SI3): each run not ended, and each ended one
// while its status.finishedAt is less than endSettle ago, so a panel joins the gateway's last
// readings of a short run to its payer. A run that ended without a finishedAt is not named: no
// meter, a new leader included, could tell how long ago.
func (m *Meter) chargeInfo(all []runs.Run) []runs.Run {
	now := m.now()
	var out []runs.Run
	for _, r := range all {
		if !runs.Terminal(r.Phase) || (!r.Finished.IsZero() && now.Sub(r.Finished) < endSettle) {
			out = append(out, r)
		}
	}
	return out
}

// charged are the ledger keys a run's tokens settle to: the fleet and the person it is charged to
// (v2 per-person caps D2), else its principal alone.
func charged(r runs.Run) []string {
	if r.ChargedTo != "" {
		return []string{runs.PrincipalFactory, r.ChargedTo}
	}
	return []string{r.Principal}
}

// daySpent reports whether any key the run is charged to has spent its day: budget-principal's
// test, a person's day included (D5). A nil spent column reads nothing spent.
//
// Unless EnforcePrincipal (R3's shadow), only a sponsored principal's day counts: it is a bound
// whatever that flag says (F4, R7).
func (m *Meter) daySpent(r runs.Run, spent map[string]int64) bool {
	for _, k := range charged(r) {
		if !m.Budgets.EnforcePrincipal && !m.Budgets.Sponsored(k) {
			continue
		}
		if spent != nil && spent[k] >= m.dailyCap(k) {
			return true
		}
	}
	return false
}

// revocation names why a live run must stop, and the limit that fired, in the order the design
// lists the caps (C5). A run capped in USD is not held to its MaxTokens: pricing cache reads at
// their own rate is the point (OD-3), and B1 bounds the same money at the gateway.
func (m *Meter) revocation(r runs.Run, tokens, b1, usd, capUSD int64, throttled bool, daySpent bool) (reason, limit string) {
	switch {
	case r.MaxMicroUSD <= 0 && m.Budgets.Sponsored(r.ChargedTo): // F4, N-1: fail closed, never on the tier's cap
		return ReasonBudgetRun, LimitRunUSD
	case capUSD > 0 && usd >= capUSD:
		return ReasonBudgetRun, LimitRunUSD
	case capUSD <= 0 && r.MaxTokens > 0 && tokens >= r.MaxTokens:
		return ReasonBudgetRun, LimitRunTokens
	case throttled && m.B1Ceiling > 0 && b1 >= m.B1Ceiling:
		return ReasonBudgetRun, LimitGatewayCeiling
	case throttled:
		return ReasonBudgetFleet, LimitFleet // B2, the only other bucket on agent-router (R13)
	case daySpent:
		return ReasonBudgetPrincipal, LimitPrincipalDay
	}
	return "", ""
}

// b1Spend is what the gateway's B1 bucket holds for a run, as far as the meter can tell: B1 charges
// a priced call the meter's own price in reference tokens (ai-native-ref's policy-budgets.yaml,
// generated from meter.prices), and an unpriced one its raw tokens. Micro-USD over the reference
// rate ($1.70 per M, 1.7 micro-USD a token) is reference tokens.
func b1Spend(tokens int64, usd map[string]int64, id string) int64 {
	if v, priced := usd[id]; priced {
		return int64(float64(v) / config.RefUSDPerMToken)
	}
	return tokens
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

// excludesCache names, once per run, a model whose input read below its cache tokens: a gateway
// path that reports input without them changes how the run is priced, so it shows on a counter.
func (m *Meter) excludesCache(ctx context.Context, r runs.Run, model string) {
	k := r.ID + "/input-excludes-cache/" + model
	if m.named[k] {
		return
	}
	if m.named == nil {
		m.named = map[string]bool{}
	}
	m.named[k] = true
	m.log().Info("the gateway reported a model's input without its cache tokens", "run", r.ID, "model", model)
	if m.OnInputExcludesCache != nil {
		m.OnInputExcludesCache(ctx, model)
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

func (m *Meter) dailyCap(principal string) int64 { return m.Budgets.DailyMicroUSD(principal) }

// tellDay hands Day every principal in day's ledger. At a day's first tick it first tells each
// principal of the day before that is not in it yet a spend of 0: a gauge moves only when
// recorded, and an 80% alert must not outlive midnight (TC8).
func (m *Meter) tellDay(day string, spent map[string]int64) {
	if m.toldDay != day {
		for p := range m.told {
			if _, ok := spent[p]; !ok {
				m.Day(p, 0, m.dailyCap(p))
			}
		}
		m.told, m.toldDay = map[string]bool{}, day
	}
	for p, n := range spent {
		m.Day(p, n, m.dailyCap(p))
		m.told[p] = true
	}
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

// retainedFrom is the oldest UTC day whose ledger prune keeps: every older one is deleted.
func (m *Meter) retainedFrom() string {
	return m.now().UTC().Add(-ledgerRetention).Format(api.LedgerDayLayout)
}

// prune deletes the day ledgers older than ledgerRetention, the USD ones and the token ones they
// replaced (#155). Once per UTC day: a missed day of deletions is made up the next one, and the
// names are dated.
func (m *Meter) prune(ctx context.Context, day string) error {
	if m.pruned == day {
		return nil
	}
	var cms corev1.ConfigMapList
	if err := m.Ledger.List(ctx, &cms, client.InNamespace(m.Namespace)); err != nil {
		return fmt.Errorf("meter: list ledgers: %w", err)
	}
	cutoff := m.retainedFrom()
	var errs []error
	for i := range cms.Items {
		cm := &cms.Items[i]
		dated, ok := api.LedgerDay(cm.Name, api.LedgerPrefix)
		if !ok {
			dated, ok = api.LedgerDay(cm.Name, api.LegacyLedgerPrefix)
		}
		// A name that is not a day is not a ledger to us; the layout is ordered: so are the strings.
		if !ok || dated >= cutoff {
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
