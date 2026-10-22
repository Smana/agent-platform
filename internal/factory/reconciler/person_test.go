// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/api"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/runs"
)

func personBudgets(enabled, enforce bool) config.Budgets {
	return config.Budgets{FactoryDailyUSD: 25, HumanDailyUSD: 5, EnforcePrincipal: enforce,
		PerPerson: config.PerPerson{Enabled: enabled, DailyUSD: 5}}
}

// spentLedger is today's ledger with a spent column.
func spentLedger(spent map[string]int64) *corev1.ConfigMap {
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: api.LedgerPrefix + now.UTC().Format(api.LedgerDayLayout),
		Namespace: "agent-system"}}
	if err := api.SetLedgerSpentColumn(cm, spent); err != nil {
		panic(err)
	}
	return cm
}

func reservationOf(t *testing.T, g *rig, id string) api.Reservation {
	t.Helper()
	var cm corev1.ConfigMap
	if err := g.r.Ledger.Get(t.Context(), client.ObjectKey{Namespace: "agent-system",
		Name: api.LedgerPrefix + g.r.Now().UTC().Format(api.LedgerDayLayout)}, &cm); err != nil {
		t.Fatal(err)
	}
	var r api.Reservation
	if err := json.Unmarshal([]byte(cm.Data[api.LedgerReserved+id]), &r); err != nil {
		t.Fatalf("%s: %v %v", id, err, cm.Data)
	}
	return r
}

// D2/D3: a labelled task's run is reserved under the fleet and its labeller in one entry, its
// claim and record name the person; off, the fleet alone, as before.
func TestATaskRunIsChargedToItsLabeller(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		g := newRig(t, issueTask("3buqdlot", 7, "x"))
		g.r.Cfg.Budgets = personBudgets(enabled, true)
		tk := g.reconcile(t, "3buqdlot", 3)
		want, person := api.Reservation{Principal: runs.PrincipalFactory, MaxTokens: tk.Spec.Budget.RunTokens}, ""
		if enabled {
			want.Person, person = "github:smana", "github:smana"
		}
		if tk.Status.Phase != v1alpha1.PhaseImplementing || reservationOf(t, g, rid(0)) != want ||
			g.runs.specs[rid(0)].ChargedTo != person || tk.Status.Runs[0].ChargedTo != person ||
			g.runs.specs[rid(0)].Principal != runs.PrincipalFactory {
			t.Fatalf("enabled %v: %s %+v %+v %+v", enabled, tk.Status.Phase, reservationOf(t, g, rid(0)), g.runs.specs[rid(0)], tk.Status.Runs)
		}
	}
}

// D4/D5 at Queued: the person's day spent escalates the task naming them (budget-person); the
// fleet's day keeps its wait (nobody's fault); in shadow the run starts and the person is counted.
func TestQueuedRefusesAPersonPastTheirDay(t *testing.T) {
	for name, c := range map[string]struct {
		spent   map[string]int64
		enforce bool
		phase   string
		reason  string
		metric  string
	}{
		"person spent":         {spent: map[string]int64{"github:smana": 5_000_000}, enforce: true, phase: v1alpha1.PhaseEscalated, reason: "budget-person", metric: "revoked budget-person"},
		"fleet spent":          {spent: map[string]int64{runs.PrincipalFactory: 25_000_000}, enforce: true, phase: v1alpha1.PhaseQueued, reason: "waiting_daily_budget"},
		"person spent, shadow": {spent: map[string]int64{"github:smana": 5_000_000}, phase: v1alpha1.PhaseImplementing, metric: "shadow github:smana"},
		"another person spent": {spent: map[string]int64{"github:bob": 5_000_000}, enforce: true, phase: v1alpha1.PhaseImplementing},
	} {
		t.Run(name, func(t *testing.T) {
			g := newRig(t, issueTask("3buqdlot", 7, "x"), spentLedger(c.spent))
			g.r.Cfg.Budgets = personBudgets(true, c.enforce)
			tk := g.reconcile(t, "3buqdlot", 3)
			if tk.Status.Phase != c.phase || (c.reason != "" && tk.Status.Reason != c.reason) ||
				(c.metric != "" && !slices.Contains(g.metrics.recorded, c.metric)) {
				t.Fatalf("%s %s %q", tk.Status.Phase, tk.Status.Reason, g.metrics.recorded)
			}
			if c.reason == "budget-person" {
				say := strings.Join(g.f.Comments(7), "\n")
				if !strings.Contains(say, "`github:smana`") || !strings.Contains(say, "00:00 UTC") || len(g.runs.specs) != 0 {
					t.Fatalf("the escalation names the person and the reset, and starts nothing: %q %v", say, g.runs.specs)
				}
			}
		})
	}
}

// D2: a /factory retry charges the retrier, not the labeller, for the runs it causes.
func TestARetryIsChargedToTheRetrier(t *testing.T) {
	g := newRig(t, escalatedTask(false), roomOf("3buqdlot"))
	g.r.Cfg.Budgets = personBudgets(true, true)
	g.r.Cfg.Maintainers = append(g.r.Cfg.Maintainers, "Alice")
	g.f.SetComments(7, retryBy(77, "Alice", time.Minute))
	tk := g.reconcile(t, "3buqdlot", 1)
	if tk.Status.RetriedBy != "github:alice" {
		t.Fatalf("%q", tk.Status.RetriedBy)
	}
	tk = g.reconcile(t, "3buqdlot", 1)
	if tk.Status.Phase != v1alpha1.PhaseImplementing || tk.Status.Runs[1].ChargedTo != "github:alice" ||
		g.runs.specs[rid(1)].ChargedTo != "github:alice" || reservationOf(t, g, rid(1)).Person != "github:alice" {
		t.Fatalf("%s %+v", tk.Status.Phase, tk.Status.Runs)
	}
}
