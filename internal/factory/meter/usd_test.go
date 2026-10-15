// SPDX-License-Identifier: Apache-2.0

package meter

import (
	"context"
	"testing"

	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/runs"
)

type typed map[string]Usage

func (s typed) RunUsage(context.Context) (map[string]Usage, error) { return s, nil }

// GLM-5.3's list prices, the v2 cohort's (cloud-native-ref scripts/ops/github/cohort-row.py).
var glm = map[string]config.Price{"agent-default": {Input: 1.40, CacheRead: 0.26, Output: 4.40}}

// The v2 cohort's four arm-A implementer runs that the 1.5 M token cap revoked (#2279, #2277,
// #2278, #2269): 91–96 % of their input was cache reads.
var cohort = map[string]Usage{
	"#2279": {Input: 1_631_496, CacheRead: 1_552_384, Output: 30_304},
	"#2277": {Input: 1_541_445, CacheRead: 1_469_248, Output: 48_755},
	"#2278": {Input: 1_690_907, CacheRead: 1_619_776, Output: 27_722},
	"#2269": {Input: 1_594_321, CacheRead: 1_507_328, Output: 15_932},
}

func typedUsage(u Usage) Usage {
	u.Tokens, u.Typed = u.Input+u.Output, true
	return u
}

func standardRun(id string) runs.Run {
	return runs.Run{ID: id, Phase: "Running", Tier: "standard", Model: "agent-default", MaxTokens: 1_500_000}
}

type revocation struct{ reason, limit string }

func usdMeter(st *store, src Source, revoked *[]revocation, fell *[]string) *Meter {
	return &Meter{Runs: st, Source: src, Prices: glm,
		RunUSD: map[string]float64{"standard": config.DefaultRunUSD(1_500_000)},
		OnRevoke: func(_ context.Context, r, l string) {
			*revoked = append(*revoked, revocation{r, l})
		},
		OnFallback: func(_ context.Context, why string) { *fell = append(*fell, why) }}
}

// J-AC1 under OD-6 (b): the standard tier's default cap is $2.55, and none of the four cohort runs
// reaches it, though each is past the 1.5 M tokens that revoked it.
func TestCohortRunsStayUnderTheStandardUSDCap(t *testing.T) {
	if c := config.DefaultRunUSD(1_500_000); c != 2.55 {
		t.Fatalf("the standard tier's default cap is $%v, want $2.55", c)
	}
	for issue, u := range cohort {
		t.Run(issue, func(t *testing.T) {
			usd := float64(cost(glm["agent-default"], typedUsage(u))) / 1e6
			if usd < 0.58 || usd > 0.70 { // the cohort's $0.58–0.70, or 0.42 M reference tokens at most
				t.Errorf("cost $%.4f, want $0.58–0.70", usd)
			}
			st := &store{patches: map[string]map[string]string{}, runs: []runs.Run{standardRun("aaaaaaaa")}}
			var revoked []revocation
			var fell []string
			if err := usdMeter(st, typed{"aaaaaaaa": typedUsage(u)}, &revoked, &fell).Tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			p := st.patches["aaaaaaaa"]
			if p[runs.AnnRevoked] != "" || len(revoked) != 0 || len(fell) != 0 {
				t.Errorf("revoked at $%.4f of $2.55: %v %v %v", usd, p, revoked, fell)
			}
			if p[runs.AnnUsage] == "" || u.Input+u.Output <= 1_500_000 {
				t.Errorf("the token count is still written, and past the old cap: %v", p)
			}
		})
	}
}

// The same volume with no cache hit costs over three times as much ($2.30–2.49), and the cap still bounds
// it: an uncached run is revoked once its cost reaches $2.55, about 1.8 M tokens.
func TestAnUncachedRunIsStillBounded(t *testing.T) {
	for issue, u := range cohort {
		cached := cost(glm["agent-default"], typedUsage(u))
		u.CacheRead = 0
		if fresh := cost(glm["agent-default"], typedUsage(u)); float64(fresh) < 3*float64(cached) || fresh >= 2_550_000 {
			t.Errorf("%s uncached: $%.4f, want over 3 × $%.4f and under the cap", issue, float64(fresh)/1e6, float64(cached)/1e6)
		}
	}
	st := &store{patches: map[string]map[string]string{}, runs: []runs.Run{standardRun("aaaaaaaa")}}
	src := typed{}
	var revoked []revocation
	var fell []string
	m := usdMeter(st, src, &revoked, &fell)
	for _, in := range []int64{900_000, 1_800_000} { // $2.52 + output's $0.13: over
		src["aaaaaaaa"] = typedUsage(Usage{Input: in, Output: 30_000})
		if err := m.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if len(revoked) != 1 || revoked[0] != (revocation{ReasonBudgetRun, LimitRunUSD}) {
		t.Fatalf("revocations %v, want one budget-run on the USD cap", revoked)
	}
}

// One release of compatibility: a tier without runUSD keeps its token cap, prices or not.
func TestATierWithoutAUSDCapKeepsItsTokenCap(t *testing.T) {
	st := &store{patches: map[string]map[string]string{}, runs: []runs.Run{standardRun("aaaaaaaa")}}
	var revoked []revocation
	var fell []string
	m := usdMeter(st, typed{"aaaaaaaa": typedUsage(cohort["#2279"])}, &revoked, &fell)
	m.RunUSD = nil
	if err := m.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(revoked) != 1 || revoked[0] != (revocation{ReasonBudgetRun, LimitRunTokens}) || len(fell) != 0 {
		t.Fatalf("revocations %v, fallbacks %v", revoked, fell)
	}
}

// A run its USD cap cannot price is capped in tokens, and says so once: never metered at $0.
func TestAnUnpricableRunFallsBackToItsTokenCap(t *testing.T) {
	for name, c := range map[string]struct {
		model string
		u     Usage
		why   string
	}{
		"an unpriced model": {"tier-standard", typedUsage(cohort["#2279"]), FallbackUnpriced},
		"an untyped query":  {"agent-default", Usage{Tokens: 1_661_800}, FallbackUntyped},
	} {
		t.Run(name, func(t *testing.T) {
			r := standardRun("aaaaaaaa")
			r.Model = c.model
			r.MaxTokens = 2_000_000
			st := &store{patches: map[string]map[string]string{}, runs: []runs.Run{r}}
			src := typed{"aaaaaaaa": c.u}
			var revoked []revocation
			var fell []string
			m := usdMeter(st, src, &revoked, &fell)
			for range 2 {
				if err := m.Tick(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			if len(fell) != 1 || fell[0] != c.why || len(revoked) != 0 {
				t.Fatalf("fallbacks %v (want one %s), revocations %v", fell, c.why, revoked)
			}
			u := c.u
			u.Tokens, u.Input = 2_000_000, 2_000_000
			src["aaaaaaaa"] = u
			if err := m.Tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			if len(revoked) != 1 || revoked[0] != (revocation{ReasonBudgetRun, LimitRunTokens}) {
				t.Fatalf("revocations %v, want one on the token cap", revoked)
			}
		})
	}
}

// R12 for dollars: a gateway restart zeroes the typed counters, and the run's cost keeps growing
// from its high-water mark; the end reports it once.
func TestACounterResetNeverResetsTheUSDCost(t *testing.T) {
	st := &store{patches: map[string]map[string]string{}, runs: []runs.Run{standardRun("aaaaaaaa")}}
	src := typed{}
	var revoked []revocation
	var fell []string
	var costs []float64
	m := usdMeter(st, src, &revoked, &fell)
	m.OnCost = func(_ context.Context, tier string, usd float64) {
		if tier == "standard" {
			costs = append(costs, usd)
		}
	}
	for _, in := range []int64{1_000_000, 100_000, 1_000_000} { // $1.40, a reset, then +$1.26: $2.66
		src["aaaaaaaa"] = typedUsage(Usage{Input: in})
		if err := m.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if len(revoked) != 1 || revoked[0].limit != LimitRunUSD {
		t.Fatalf("revocations %v, want the USD cap past the reset", revoked)
	}
	st.runs[0].Phase = "BudgetExhausted"
	for range 2 {
		if err := m.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if len(costs) != 1 || costs[0] != 2.66 {
		t.Fatalf("costs %v, want one $2.66", costs)
	}
}
