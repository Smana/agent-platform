// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	roomv1 "github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/api"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/rooms"
	"github.com/Smana/agent-platform/internal/factory/triage"
)

// sponsor makes tk an api task of system:slack-app's under a ceiling.
func sponsor(tk *v1alpha1.Task, maxMicroUSD int64) {
	tk.Spec.Source = v1alpha1.Source{Kind: "api", Ref: "api:slack-app", Key: "k", RequestedBy: "system:slack-app",
		Sponsor: "github:smana", OnBehalfOf: "slack:T01ABC/U02DEF", Trust: "untrusted", ContentSHA256: strings.Repeat("a", 64)}
	tk.Spec.MaxMicroUSD = maxMicroUSD
}

// priceTiers gives the rig's tiers the host's USD caps (standard $2.55/$5.10), as a config with
// meter.prices has them: api.sponsored requires prices (C13).
func priceTiers(g *rig) {
	for name, usd := range map[string][2]float64{"standard": {2.55, 5.10}, "frontier": {6.80, 13.60}} {
		tr := g.r.Cfg.Tiers[name]
		tr.RunUSD, tr.TaskUSD = usd[0], usd[1]
		g.r.Cfg.Tiers[name] = tr
	}
	g.r.Triage = triage.Static{Cfg: g.r.Cfg}
}

// R5, R7, R8, C4: an api task is capped under its ceiling with room for three runs, in USD and in
// tokens, charged to its principal with per-person caps off; its principal watches its room, and
// the snapshot names both identities.
func TestSponsoredTaskEndToEnd(t *testing.T) {
	tk := issueTask("3buqdlot", 0, "Fix the dead link in docs/a.md.")
	sponsor(tk, 2_000_000)
	g := newRig(t, tk) // per-person caps off, as the default config ships
	priceTiers(g)
	got := g.reconcile(t, "3buqdlot", 3) // Received → Triaged → Queued → Implementing
	if got.Status.Phase != v1alpha1.PhaseImplementing {
		t.Fatalf("%s %s", got.Status.Phase, got.Status.Reason)
	}
	if b := got.Spec.Budget; b.TaskMicroUSD != 2_000_000 || b.RunMicroUSD != 666_666 || b.TaskTokens != 1_176_470 || b.RunTokens != 392_156 {
		t.Fatalf("the ceiling: %+v", b)
	}
	if s := g.runs.specs[rid(0)]; s.ChargedTo != "system:slack-app" {
		t.Fatalf("charged to %q", s.ChargedTo)
	}
	var room roomv1.Room
	if err := g.c.Get(t.Context(), types.NamespacedName{Namespace: "agent-system", Name: "3buqdlot"}, &room); err != nil {
		t.Fatal(err)
	}
	if len(room.Spec.Members) != 1 || room.Spec.Members[0] != (roomv1.Member{Principal: "system:slack-app", Role: "watcher"}) {
		t.Fatalf("members %+v", room.Spec.Members)
	}
	snapshot := ""
	for _, s := range g.log.states {
		if strings.Contains(s, "the original task, snapshotted") {
			snapshot = s
		}
	}
	if !strings.Contains(snapshot, "Requested by system:slack-app, sponsored by github:smana, on behalf of slack:T01ABC/U02DEF; ceiling $2.00.") {
		t.Fatalf("the snapshot names both identities:\n%s", snapshot)
	}
}

// R5: a ceiling above the class's caps leaves the class's, in USD and in tokens.
func TestCeilingNeverRaisesTheClassCap(t *testing.T) {
	tk := issueTask("3buqdlot", 0, "Fix the dead link in docs/a.md.")
	sponsor(tk, 1_000_000_000)
	g := newRig(t, tk)
	priceTiers(g)
	got := g.reconcile(t, "3buqdlot", 1) // Received → Triaged
	if b := got.Spec.Budget; b.TaskMicroUSD != 5_100_000 || b.RunMicroUSD != 2_550_000 || b.TaskTokens != 3_000_000 || b.RunTokens != 1_500_000 {
		t.Fatalf("%+v", b)
	}
}

// C10: a titled api task waits up to two minutes for the issue its intake files, as RunLore's does.
func TestATitledAPITaskWaitsForItsIssue(t *testing.T) {
	tk := issueTask("3buqdlot", 0, "# docs(agents): the link is dead\n\n## What is wrong\nx")
	sponsor(tk, 2_000_000)
	g := newRig(t, tk)
	priceTiers(g)
	if got := g.reconcile(t, "3buqdlot", 1); got.Spec.Budget.Tier != "" {
		t.Fatalf("triaged before its issue: %+v", got.Spec.Budget)
	}
	g.r.Now = func() time.Time { return now.Add(2 * time.Minute) } // three minutes after its creation
	if got := g.reconcile(t, "3buqdlot", 1); got.Spec.Budget.Tier != "standard" {
		t.Fatalf("still waiting past two minutes: %+v", got.Spec.Budget)
	}
}

// C9: an api task is never offered a split: its children would escape its ceiling.
func TestAnAPITaskIsNeverSplit(t *testing.T) {
	tk := splitRigWith(t, "frontier", false, func(tk *v1alpha1.Task) { sponsor(tk, 13_000_000) }).reconcile(t, "3buqdlot", 1)
	if tk.Spec.Split != nil {
		t.Fatalf("an api task offered a split: %+v", tk.Spec.Split)
	}
}

// C7: a room is adopted when it holds each member asked for, whatever else its members are; the
// factory's other callers ask for none and adopt as before.
func TestEnsureAdoptsARoomHoldingTheMember(t *testing.T) {
	g := newRig(t)
	w := roomv1.Member{Principal: "system:slack-app", Role: "watcher"}
	ensure := func(ms ...roomv1.Member) error {
		return rooms.Ensure(t.Context(), g.c, "agent-system", "3buqdlot", "public", "Smana/cloud-native-ref", ms...)
	}
	if err := ensure(w); err != nil {
		t.Fatal(err)
	}
	if err := ensure(w); err != nil {
		t.Fatalf("a replay: %v", err)
	}
	if err := ensure(); err != nil {
		t.Fatalf("no member asked: %v", err)
	}
	if err := ensure(roomv1.Member{Principal: "system:other", Role: "watcher"}); !errors.Is(err, rooms.ErrForeignRoom) {
		t.Fatalf("a room without the member: %v", err)
	}
}

const sponsoredDayUSD = 3.0

// sponsoredDayRig is a rig with a sponsored Task whose principal has a $3 day, the shadow flag
// (budgets.enforcePrincipal) off, and its triage done: the next reconcile admits its run.
func sponsoredDayRig(t *testing.T, factoryDailyUSD float64, objs ...*v1alpha1.Task) *rig {
	t.Helper()
	g := newRig(t)
	for _, tk := range objs {
		sponsor(tk, 2_000_000)
		if err := g.c.Create(t.Context(), tk); err != nil {
			t.Fatal(err)
		}
	}
	priceTiers(g)
	g.r.Cfg.API.Sponsored = []config.Sponsored{{Name: "slack-app", DailyUSD: sponsoredDayUSD}}
	g.r.Cfg.Budgets = config.Budgets{FactoryDailyUSD: factoryDailyUSD, HumanDailyUSD: 8.5, EnforcePrincipal: false}.
		WithSponsoredDay("system:slack-app", sponsoredDayUSD)
	return g
}

// holdDay reserves what is left of the principal's day under another run id.
func holdDay(t *testing.T, g *rig, micro int64) {
	t.Helper()
	if _, err := api.Reserve(t.Context(), g.c, "agent-system", now, "zzzzzzzz",
		api.Reservation{Principal: "system:factory", Person: "system:slack-app", MaxMicroUSD: micro}, 1_000_000_000, 0, false); err != nil {
		t.Fatal(err)
	}
}

// R7, F4, C25 (ledger ruling I1): a sponsored principal's day is a bound whatever
// budgets.enforcePrincipal says, and whatever the fleet's own day: a run that does not fit is
// refused and never starts.
func TestASponsoredRunIsRefusedWhenThePrincipalsDayIsFull(t *testing.T) {
	for name, c := range map[string]struct {
		factoryDailyUSD float64
		hold            func(*testing.T, *rig)
	}{
		"reservations hold the day":           {25, func(t *testing.T, g *rig) { holdDay(t, g, 3_000_000) }},
		"the day's spend fills it":            {25, func(t *testing.T, g *rig) { settleSpend(t, g, 3_000_000) }},
		"reservations, the fleet's day unset": {0, func(t *testing.T, g *rig) { holdDay(t, g, 3_000_000) }},
		"one run short":                       {25, func(t *testing.T, g *rig) { holdDay(t, g, 3_000_000-666_666+1) }},
	} {
		t.Run(name, func(t *testing.T) {
			g := sponsoredDayRig(t, c.factoryDailyUSD, issueTask("3buqdlot", 0, "Fix the dead link in docs/a.md."))
			c.hold(t, g)
			got := g.reconcile(t, "3buqdlot", 3)
			if got.Status.Phase == v1alpha1.PhaseImplementing || len(g.runs.specs) != 0 {
				t.Fatalf("a run started past the principal's day: %s %s, %d specs", got.Status.Phase, got.Status.Reason, len(g.runs.specs))
			}
			if got.Status.Phase != v1alpha1.PhaseEscalated || got.Status.Reason != "budget-person" {
				t.Fatalf("%s %s", got.Status.Phase, got.Status.Reason)
			}
		})
	}
}

// settleSpend writes the principal's settled spend into the day's ledger, as the meter does.
func settleSpend(t *testing.T, g *rig, micro int64) {
	t.Helper()
	key := types.NamespacedName{Namespace: "agent-system", Name: api.LedgerPrefix + now.UTC().Format(api.LedgerDayLayout)}
	var cm corev1.ConfigMap
	err := g.c.Get(t.Context(), key, &cm)
	found := err == nil
	if !found {
		cm = corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}}
	}
	if err := api.SetLedgerSpentColumn(&cm, map[string]int64{"system:slack-app": micro}); err != nil {
		t.Fatal(err)
	}
	if found {
		err = g.c.Update(t.Context(), &cm)
	} else {
		err = g.c.Create(t.Context(), &cm)
	}
	if err != nil {
		t.Fatal(err)
	}
}

// A run that fits is admitted, and its reservation is charged to the principal's day.
func TestASponsoredRunThatFitsIsAdmittedAndCharged(t *testing.T) {
	g := sponsoredDayRig(t, 25, issueTask("3buqdlot", 0, "Fix the dead link in docs/a.md."))
	holdDay(t, g, 3_000_000-666_666) // exactly one run left
	if got := g.reconcile(t, "3buqdlot", 3); got.Status.Phase != v1alpha1.PhaseImplementing {
		t.Fatalf("%s %s", got.Status.Phase, got.Status.Reason)
	}
	used, err := api.DayUsed(t.Context(), g.c, "agent-system", "system:slack-app", now)
	if err != nil || used != 3_000_000 {
		t.Fatalf("used %d, %v", used, err)
	}
}

// R50, F4: two runs of two different Tasks of one sponsored principal race for its last slot; the
// ledger's conflict-checked write admits exactly one, whatever the interleaving.
func TestTwoSponsoredRunsRaceForTheLastSlot(t *testing.T) {
	g := sponsoredDayRig(t, 25)
	holdDay(t, g, 3_000_000-666_666) // one run's room left
	var (
		wg            sync.WaitGroup
		mu            sync.Mutex
		landed, refus int
	)
	for _, id := range []string{"aaaaaaaa", "bbbbbbbb"} {
		tk := issueTask("3buqdlot", 0, "x")
		tk.Name = id
		sponsor(tk, 2_000_000)
		tk.Spec.Budget = v1alpha1.Budget{RunMicroUSD: 666_666}
		wg.Go(func() {
			for { // the reconciler requeues ErrLedgerBusy
				_, err := g.r.reserveFactoryDay(t.Context(), tk, g.r.Cfg.Budgets.DailyMicroUSD("system:factory"))
				mu.Lock()
				switch {
				case errors.Is(err, api.ErrLedgerBusy):
					mu.Unlock()
					continue
				case err == nil:
					landed++
				case errors.Is(err, api.ErrPersonOverBudget):
					refus++
				default:
					t.Error(err)
				}
				mu.Unlock()
				return
			}
		})
	}
	wg.Wait()
	if landed != 1 || refus != 1 {
		t.Fatalf("%d admitted, %d refused", landed, refus)
	}
}
