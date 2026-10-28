// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"testing"

	v1alpha1 "github.com/Smana/maku/api/factory/v1alpha1"
	"github.com/Smana/maku/internal/factory/config"
	"github.com/Smana/maku/internal/factory/triage"
)

// TC7: a factory run reserves its USD run cap in the day ledger; a tier capped in tokens reserves
// its runTokens at the reference rate, 1.7 micro-USD a token.
func TestAFactoryRunReservesItsRunCapInMicroUSD(t *testing.T) {
	for name, c := range map[string]struct {
		runUSD float64
		want   int64
	}{
		"a USD run cap": {2.00, 2_000_000},
		"a token cap":   {0, 2_550_000},
	} {
		t.Run(name, func(t *testing.T) {
			g := newRig(t, issueTask("3buqdlot", 7, "x"))
			cf := cfg()
			tier := cf.Tiers["standard"]
			tier.RunUSD = c.runUSD
			cf.Tiers["standard"] = tier
			g.r.Triage = triage.Static{Cfg: cf}
			g.r.Cfg.Budgets = config.Budgets{FactoryDailyUSD: 42.50, HumanDailyUSD: 8.50}
			tk := g.reconcile(t, "3buqdlot", 3)
			if got := reservationOf(t, g, rid(0)); tk.Status.Phase != v1alpha1.PhaseImplementing || got.MaxMicroUSD != c.want {
				t.Fatalf("%s: reserved %+v, want %d micro-USD", tk.Status.Phase, got, c.want)
			}
		})
	}
}
