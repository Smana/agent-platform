// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"math"
	"slices"
	"testing"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/meter"
)

type fakeCosts map[string]meter.Cost

func (f fakeCosts) Get(id string) (meter.Cost, bool) { c, ok := f[id]; return c, ok }

// OD-6 (b) for the task: a pair task's runs, 95 % cache reads, pass 4.5 M tokens and stay far
// under the standard tier's $5.10; the same volume uncached leaves no room for another $2.55 run.
// A run the meter cannot price holds the task to its 3 M tokens, said once.
func TestTaskUSDCapPricesCacheReads(t *testing.T) {
	glm := func(in, cached, out int64) int64 { // GLM-5.3 list, micro-USD
		return int64(math.Round(float64(in-cached)*1.40 + float64(cached)*0.26 + float64(out)*4.40))
	}
	type run struct {
		role, trigger   string
		in, cached, out int64
	}
	pair := []run{
		{"implementer", "initial", 1_631_496, 1_552_384, 30_304}, // the cohort's #2279
		{"reviewer", "review", 1_400_000, 1_316_000, 20_000},     // 94 % cached
		{"implementer", "human", 1_594_321, 1_507_328, 15_932},   // a revision the size of #2269
	}
	for name, c := range map[string]struct {
		uncached bool
		why      string
		started  bool
		want     []string
	}{
		"95 % cached: $1.78 of $5.10":    {false, "", true, nil},
		"the same volume uncached":       {true, "", false, []string{"revoked budget-task", "limit task-usd"}},
		"an unpriced run: the token cap": {false, meter.FallbackUnpriced, false, []string{"fallback task unpriced", "revoked budget-task", "limit task-tokens"}},
	} {
		t.Run(name, func(t *testing.T) {
			tk := awaiting()
			tk.Spec.Template = "pair"
			tk.Spec.Budget = v1alpha1.Budget{Tier: "standard", Model: "agent-default", RunTokens: 1_500_000, TaskTokens: 3_000_000,
				RunMinutes: 45, RunMicroUSD: 2_550_000, TaskMicroUSD: 5_100_000}
			tk.Status.Phase, tk.Status.NextTrigger = v1alpha1.PhaseQueued, "human"
			costs := fakeCosts{}
			tk.Status.Runs = nil
			var tokens int64
			for i, r := range pair {
				cached := r.cached
				if c.uncached {
					cached = 0
				}
				tk.Status.Runs = append(tk.Status.Runs, v1alpha1.RunRecord{ID: rid(i), Role: r.role, Trigger: r.trigger,
					Phase: "Succeeded", Tokens: r.in + r.out, Started: awaiting().Status.Runs[0].Started})
				costs[rid(i)] = meter.Cost{MicroUSD: glm(r.in, cached, r.out)}
				tokens += r.in + r.out
			}
			if c.why != "" {
				costs[rid(1)] = meter.Cost{Why: c.why}
			}
			if tokens < 4_500_000 {
				t.Fatalf("the pair spends %d tokens, want 4.5 M or more", tokens)
			}
			g := newRig(t, tk, roomOf("3buqdlot"))
			g.f.SetPR(pr12())
			g.r.Cfg.Budgets.EnforceTask = true
			g.r.Costs = costs
			got := g.reconcile(t, "3buqdlot", 1)
			if _, started := g.runs.specs[rid(3)]; started != c.started {
				t.Fatalf("the revision started %v, want %v: %s %s, cost %d µ$", started, c.started,
					got.Status.Phase, got.Status.Reason, got.Status.Usage.CostMicroUSD)
			}
			if !c.started && (got.Status.Phase != v1alpha1.PhaseEscalated || got.Status.Reason != "budget-task") {
				t.Fatalf("%s %s", got.Status.Phase, got.Status.Reason)
			}
			for _, w := range c.want {
				if !slices.Contains(g.metrics.recorded, w) {
					t.Errorf("missing %q in %q", w, g.metrics.recorded)
				}
			}
			if n := countOf(g.metrics.recorded, "fallback task unpriced"); c.why != "" && n != 1 {
				t.Errorf("the fallback is said %d times, want once", n)
			}
		})
	}
}

func countOf(s []string, v string) int {
	n := 0
	for _, x := range s {
		if x == v {
			n++
		}
	}
	return n
}
