// SPDX-License-Identifier: Apache-2.0

package intake

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Smana/agent-platform/internal/factory/api"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/forge/forgetest"
)

// v2 per-person caps D4 (G1): a labeller whose day is spent is refused at intake: a comment naming
// them, their cap and the reset, no Task, the label removed, the refusal counted. Off, in shadow,
// or with day left, the label starts its task as before.
func TestALabellerPastTheirDayIsRefusedAtIntake(t *testing.T) {
	for name, c := range map[string]struct {
		enabled, enforce bool
		spent            int64
		refused          bool
	}{
		"spent, enforced": {enabled: true, enforce: true, spent: 5_000_000, refused: true},
		"spent, shadow":   {enabled: true, spent: 5_000_000},
		"spent, off":      {enforce: true, spent: 5_000_000},
		"day left":        {enabled: true, enforce: true, spent: 4_999_999},
		"over the day":    {enabled: true, enforce: true, spent: 6_000_000, refused: true},
	} {
		t.Run(name, func(t *testing.T) {
			f := forgetest.NewFake()
			labelled(f, 7, "Smana")
			l := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: api.LedgerPrefix + t0.Format(api.LedgerDayLayout), Namespace: "agent-system"}}
			if err := api.SetLedgerSpentColumn(l, map[string]int64{"github:smana": c.spent}); err != nil {
				t.Fatal(err)
			}
			p, k := poller(t, f, l)
			p.Ledger, p.Now = k, func() time.Time { return t0 }
			p.Cfg.Budgets = config.Budgets{FactoryDailyUSD: 25, HumanDailyUSD: 5, EnforcePrincipal: c.enforce,
				PerPerson: config.PerPerson{Enabled: c.enabled, DailyUSD: 5}}
			if err := p.Poll(t.Context()); err != nil {
				t.Fatal(err)
			}
			got := len(tasks(t, k)) == 0
			say := strings.Join(f.Comments(7), "\n")
			refusals := p.Errors.(*counter).refused
			if got != c.refused || len(f.Removed(7)) != 1 {
				t.Fatalf("refused %v, removed %q", got, f.Removed(7))
			}
			if c.refused && (!strings.Contains(say, "`github:smana` ($5.00) is spent") || !strings.Contains(say, "00:00 UTC") ||
				len(refusals) != 1 || refusals[0] != "person_budget_exhausted github:smana") { // spend per identity SI4: the labeller
				t.Fatalf("%q %q", say, refusals)
			}
			if !c.refused && (say != "" || len(refusals) != 0) {
				t.Fatalf("nothing said: %q %q", say, refusals)
			}
		})
	}
}
