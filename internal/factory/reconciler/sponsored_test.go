// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

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
	g := sponsoredDayRig(t, tk)          // per-person caps off, as the default config ships
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

// sponsoredDayMicro is system:slack-app's day in the fixture config ($10).
const sponsoredDayMicro = 10_000_000

// sponsoredConfig is the parsed fixture: system:slack-app with a $10 day, and budgets.enforceTask
// and budgets.enforcePrincipal off, as the default config ships.
func sponsoredConfig(t *testing.T) *config.Config {
	t.Helper()
	c, err := config.Load("../config/testdata/sponsored.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// sponsoredDayRig is a rig with sponsored Tasks and the parsed fixture's budgets and principals.
func sponsoredDayRig(t *testing.T, objs ...*v1alpha1.Task) *rig {
	t.Helper()
	g := newRig(t)
	for _, tk := range objs {
		sponsor(tk, 2_000_000)
		if err := g.c.Create(t.Context(), tk); err != nil {
			t.Fatal(err)
		}
	}
	priceTiers(g)
	lc := sponsoredConfig(t)
	g.r.Cfg.API, g.r.Cfg.Budgets = lc.API, lc.Budgets
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
		hold func(*testing.T, *rig)
	}{
		"reservations hold the day": {func(t *testing.T, g *rig) { holdDay(t, g, sponsoredDayMicro) }},
		"the day's spend fills it":  {func(t *testing.T, g *rig) { settleSpend(t, g, sponsoredDayMicro) }},
		"one run short":             {func(t *testing.T, g *rig) { holdDay(t, g, sponsoredDayMicro-666_666+1) }},
	} {
		t.Run(name, func(t *testing.T) {
			g := sponsoredDayRig(t, issueTask("3buqdlot", 0, "Fix the dead link in docs/a.md."))
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
	g := sponsoredDayRig(t, issueTask("3buqdlot", 0, "Fix the dead link in docs/a.md."))
	holdDay(t, g, sponsoredDayMicro-666_666) // exactly one run left
	if got := g.reconcile(t, "3buqdlot", 3); got.Status.Phase != v1alpha1.PhaseImplementing {
		t.Fatalf("%s %s", got.Status.Phase, got.Status.Reason)
	}
	used, err := api.DayUsed(t.Context(), g.c, "agent-system", "system:slack-app", now)
	if err != nil || used != sponsoredDayMicro {
		t.Fatalf("used %d, %v", used, err)
	}
}

// R50, F4: two runs of two different Tasks of one sponsored principal race for its last slot. A
// barrier holds both until each has read the ledger, so both see the slot free: the write's
// conflict check admits exactly one, and the loser re-reads and is refused.
func TestTwoSponsoredRunsRaceForTheLastSlot(t *testing.T) {
	g := sponsoredDayRig(t)
	holdDay(t, g, sponsoredDayMicro-666_666) // one run's room left
	var (
		mu        sync.Mutex
		gets      int
		conflicts int
		both      = make(chan struct{})
	)
	ledger := func(o client.Object) bool { return strings.HasPrefix(o.GetName(), api.LedgerPrefix) }
	g.r.Ledger = interceptor.NewClient(g.c.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, o client.Object, opts ...client.GetOption) error {
			err := c.Get(ctx, key, o, opts...)
			if strings.HasPrefix(key.Name, api.LedgerPrefix) && err == nil {
				mu.Lock()
				if gets++; gets == 2 {
					close(both)
				}
				mu.Unlock()
			}
			return err
		},
		Update: func(ctx context.Context, c client.WithWatch, o client.Object, opts ...client.UpdateOption) error {
			if ledger(o) {
				select {
				case <-both:
				case <-time.After(5 * time.Second):
					t.Error("the barrier: the second run never read the ledger")
				}
			}
			err := c.Update(ctx, o, opts...)
			if apierrors.IsConflict(err) {
				mu.Lock()
				conflicts++
				mu.Unlock()
			}
			return err
		}})
	var (
		wg            sync.WaitGroup
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
	if landed != 1 || refus != 1 || conflicts < 1 {
		t.Fatalf("%d admitted, %d refused, %d conflicts", landed, refus, conflicts)
	}
}

// F4, C-1: an api task's run carries its own USD cap, a third of the ceiling; a run of any other
// task carries none, so the meter's behaviour for it is the tier's alone.
func TestARunCarriesItsOwnUSDCap(t *testing.T) {
	g := sponsoredDayRig(t, issueTask("3buqdlot", 0, "Fix the dead link in docs/a.md."))
	if got := g.reconcile(t, "3buqdlot", 3); got.Status.Phase != v1alpha1.PhaseImplementing {
		t.Fatalf("%s %s", got.Status.Phase, got.Status.Reason)
	}
	if s := g.runs.specs[rid(0)]; s.MaxMicroUSD != 666_666 {
		t.Fatalf("max micro-USD %d", s.MaxMicroUSD)
	}
	h := newRig(t, issueTask("3buqdlot", 7, "x"))
	h.reconcile(t, "3buqdlot", 3)
	if s := h.runs.specs[rid(0)]; s.MaxMicroUSD != 0 {
		t.Fatalf("an issue task's run carries a cap: %d", s.MaxMicroUSD)
	}
}

// F4, I-3: an api task's task cap is enforced whatever budgets.enforceTask says (the fixture
// ships it off); an issue task's stays a shadow there.
func TestAnAPITasksTaskCapIsEnforcedWhateverTheFlagSays(t *testing.T) {
	for name, c := range map[string]struct {
		api     bool
		started bool
	}{"an api task": {true, false}, "an issue task": {false, true}} {
		t.Run(name, func(t *testing.T) {
			tk := issueTask("3buqdlot", 7, "Fix the dead link in docs/a.md.")
			var g *rig
			if c.api {
				g = sponsoredDayRig(t, issueTaskNoIssue(tk))
			} else {
				g = newRig(t, tk)
				priceTiers(g)
				g.r.Cfg.Budgets = sponsoredConfig(t).Budgets
			}
			if g.r.Cfg.Budgets.EnforceTask {
				t.Fatal("the fixture's flag is off")
			}
			g.reconcile(t, "3buqdlot", 2) // Received → Triaged → Queued
			var cur v1alpha1.Task
			if err := g.c.Get(t.Context(), types.NamespacedName{Namespace: "agent-system", Name: "3buqdlot"}, &cur); err != nil {
				t.Fatal(err)
			}
			b := cur.Spec.Budget
			cur.Status.Usage.CostMicroUSD = b.TaskMicroUSD - b.RunMicroUSD + 1 // less than a whole run left
			if err := g.c.Status().Update(t.Context(), &cur); err != nil {
				t.Fatal(err)
			}
			got := g.reconcile(t, "3buqdlot", 1)
			_, started := g.runs.specs[rid(0)]
			if started != c.started {
				t.Fatalf("started %v, want %v: %s %s", started, c.started, got.Status.Phase, got.Status.Reason)
			}
			if !c.started && (got.Status.Phase != v1alpha1.PhaseEscalated || got.Status.Reason != "budget-task") {
				t.Fatalf("%s %s", got.Status.Phase, got.Status.Reason)
			}
		})
	}
}

// issueTaskNoIssue is tk, ready to become a sponsored task of a rig that has none yet.
func issueTaskNoIssue(tk *v1alpha1.Task) *v1alpha1.Task { tk.Spec.Issue = 0; return tk }

// M-8: an api task whose principal left the config is never run in shadow: it is escalated before
// any reservation.
func TestAnAPITaskOfAnUnknownPrincipalIsEscalated(t *testing.T) {
	g := sponsoredDayRig(t, issueTask("3buqdlot", 0, "Fix the dead link in docs/a.md."))
	g.r.Cfg.API.Sponsored = nil
	got := g.reconcile(t, "3buqdlot", 3)
	if got.Status.Phase != v1alpha1.PhaseEscalated || got.Status.Reason != "principal_unknown" || len(g.runs.specs) != 0 {
		t.Fatalf("%s %s, %d specs", got.Status.Phase, got.Status.Reason, len(g.runs.specs))
	}
	var cm corev1.ConfigMap
	key := types.NamespacedName{Namespace: "agent-system", Name: api.LedgerPrefix + now.UTC().Format(api.LedgerDayLayout)}
	if err := g.c.Get(t.Context(), key, &cm); err == nil {
		for k := range cm.Data {
			if strings.HasPrefix(k, api.LedgerReserved) {
				t.Fatalf("a reservation was made: %s", k)
			}
		}
	}
}
