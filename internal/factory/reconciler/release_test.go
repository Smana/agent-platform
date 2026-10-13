// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"slices"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/api"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/killswitch"
	"github.com/Smana/agent-platform/internal/factory/meter"
)

// seeingMeter stamps the meter's heartbeat for as long as any test's clock runs: the rig's meter
// always sees unless a test says otherwise (#54).
func seeingMeter(t *testing.T, c client.Client) {
	t.Helper()
	if err := api.StampMeter(t.Context(), c, "agent-system", now, 1000*time.Hour); err != nil {
		t.Fatal(err)
	}
}

// reservingRig is a rig whose Queued reserves each run in the day's ledger (#78), in shadow.
func reservingRig(t *testing.T) *rig {
	t.Helper()
	g := newRig(t, issueTask("3buqdlot", 7, "x"))
	g.r.Cfg.Budgets = config.Budgets{FactoryDaily: 25_000_000, HumanDaily: 5_000_000}
	return g
}

func halt(t *testing.T, g *rig, why string) {
	t.Helper()
	if why == "kill_switch" {
		if err := g.c.Create(t.Context(), &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: killswitch.ConfigMap,
			Namespace: "agent-system"}}); err != nil {
			t.Fatal(err)
		}
		return
	}
	var tk v1alpha1.Task
	if err := g.c.Get(t.Context(), keyFor("3buqdlot"), &tk); err != nil {
		t.Fatal(err)
	}
	tk.Annotations = map[string]string{v1alpha1.AnnotationStop: "true"}
	if err := g.c.Update(t.Context(), &tk); err != nil {
		t.Fatal(err)
	}
}

// #55: Queued reserves the next run before its claim exists; a task that then stops while
// Queued — no claim was ever created — releases that reservation by its run id.
func TestAStopWhileQueuedReleasesTheUnclaimedReservation(t *testing.T) {
	for _, why := range []string{"annotation", "kill_switch"} {
		t.Run(why, func(t *testing.T) {
			g := reservingRig(t)
			g.log.noRoom = true // the snapshot waits: reserved, no claim
			if tk := g.reconcile(t, "3buqdlot", 3); tk.Status.Phase != v1alpha1.PhaseQueued || tk.Status.Reason != "waiting_room_log" {
				t.Fatalf("%s %s", tk.Status.Phase, tk.Status.Reason)
			}
			if _, ids := ledgerOf(t, g); !slices.Equal(ids, []string{rid(0)}) || len(g.runs.runs) != 0 {
				t.Fatalf("reserved, unclaimed: %v %v", ids, g.runs.runs)
			}
			halt(t, g, why)
			if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Phase != v1alpha1.PhaseStopped {
				t.Fatal(tk.Status.Phase)
			}
			if used, ids := ledgerOf(t, g); len(ids) != 0 || used != 0 {
				t.Fatalf("released: used %d, %v", used, ids)
			}
		})
	}
}

// #55: a stop deletes a live claim, which never reaches the terminal phase the meter releases
// on: the stop releases it, at once, and the claim's last metered usage lands on the task before
// the claim goes — seeded at 900 on the record, 1,000 on the claim, the task settles at 1,000.
func TestAStopReleasesTheLiveRunAndKeepsItsUsage(t *testing.T) {
	g := reservingRig(t)
	g.reconcile(t, "3buqdlot", 3)
	r := g.runs.runs[rid(0)]
	r.Phase, r.Tokens = "Running", 900
	g.runs.runs[rid(0)] = r
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Usage.Tokens != 900 {
		t.Fatalf("seeded: %d", tk.Status.Usage.Tokens)
	}
	r.Tokens = 1000 // the meter's next annotation, which no observe has read yet
	g.runs.runs[rid(0)] = r
	halt(t, g, "annotation")
	tk := g.reconcile(t, "3buqdlot", 1)
	if tk.Status.Phase != v1alpha1.PhaseStopped || len(g.runs.runs) != 0 || tk.Status.Usage.Tokens != 1000 {
		t.Fatalf("%s %v %d", tk.Status.Phase, g.runs.runs, tk.Status.Usage.Tokens)
	}
	if _, ids := ledgerOf(t, g); len(ids) != 0 {
		t.Fatalf("released: %v", ids)
	}
	g.r.Now = func() time.Time { return now.Add(g.r.settleWindow()) }
	g.reconcile(t, "3buqdlot", 1)
	if !slices.Contains(g.metrics.recorded, "task_tokens 1000 standard solo review") {
		t.Fatalf("the stopped run's spend is the task's: %q", g.metrics.recorded)
	}
}

// #55: the stuck and Pending bounds delete a live claim: each releases its reservation.
func TestTheBoundsReleaseTheRunTheyDelete(t *testing.T) {
	for name, c := range map[string]struct {
		phase  string
		after  time.Duration
		reason string
	}{
		"stuck":   {"Running", StuckAfter + time.Minute, "stuck"},
		"pending": {"Pending", 31 * time.Minute, "run_unschedulable"},
	} {
		t.Run(name, func(t *testing.T) {
			g := reservingRig(t)
			g.reconcile(t, "3buqdlot", 3)
			g.runs.set(rid(0), c.phase)
			g.reconcile(t, "3buqdlot", 1) // the stuck clock starts at the first Running sight
			g.r.Now = func() time.Time { return now.Add(c.after) }
			tk := g.reconcile(t, "3buqdlot", 1)
			if tk.Status.Phase != v1alpha1.PhaseEscalated || tk.Status.Reason != c.reason || len(g.runs.runs) != 0 {
				t.Fatalf("%s %s %v", tk.Status.Phase, tk.Status.Reason, g.runs.runs)
			}
			if _, ids := ledgerOf(t, g); len(ids) != 0 {
				t.Fatalf("released: %v", ids)
			}
		})
	}
}

// #55: a release that fails, as a crash between the delete and the release would leave it,
// never holds the stop; the meter's sweep releases the unclaimed entry once its grace is past.
// A replay of the stop whose status write was lost releases what is left, once.
func TestAStopSurvivesAFailedRelease(t *testing.T) {
	g := reservingRig(t)
	g.reconcile(t, "3buqdlot", 3)
	g.runs.set(rid(0), "Running")
	ledger := g.r.Ledger
	g.r.Ledger = brokenLedger{g.c}
	halt(t, g, "annotation")
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Phase != v1alpha1.PhaseStopped || len(g.runs.runs) != 0 {
		t.Fatalf("the stop does not wait for the ledger: %s %v", tk.Status.Phase, g.runs.runs)
	}
	g.r.Ledger = ledger
	if _, ids := ledgerOf(t, g); !slices.Equal(ids, []string{rid(0)}) {
		t.Fatalf("left for the meter: %v", ids)
	}
	clock := now
	m := &meter.Meter{Runs: meterStore{g.runs}, Source: meterSource{}, Ledger: ledger, Namespace: "agent-system",
		Now: func() time.Time { return clock }}
	for _, d := range []time.Duration{0, 3 * time.Minute} {
		clock = now.Add(d)
		if err := m.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if _, ids := ledgerOf(t, g); len(ids) != 0 {
		t.Fatalf("the meter released it: %v", ids)
	}
}

// #55: a stop whose status write is lost is replayed; the replay finds the claims gone and
// still releases the task's reservations, by run id.
func TestAReplayedStopReleases(t *testing.T) {
	c := losingClient(t, func(tk *v1alpha1.Task) bool { return tk.Status.Phase == v1alpha1.PhaseStopped })
	g := newRig(t)
	g.c, g.r.Client = c, c // the ledger stays the rig's
	g.r.Cfg.Budgets = config.Budgets{FactoryDaily: 25_000_000, HumanDaily: 5_000_000}
	g.reconcile(t, "3buqdlot", 3)
	g.runs.set(rid(0), "Running")
	ledger := g.r.Ledger
	g.r.Ledger = brokenLedger{g.c} // the first stop's release fails: a crash before it
	halt(t, g, "annotation")
	if _, err := g.r.Reconcile(t.Context(), reqFor("3buqdlot")); err == nil {
		t.Fatal("the stop's status write is lost")
	}
	g.r.Ledger = ledger
	if tk := g.reconcile(t, "3buqdlot", 1); tk.Status.Phase != v1alpha1.PhaseStopped {
		t.Fatal(tk.Status.Phase)
	}
	if _, ids := ledgerOf(t, g); len(ids) != 0 {
		t.Fatalf("the replay released it: %v", ids)
	}
}

// #54: a blind meter holds the factory's next run under an enforced daily cap, and only then.
func TestABlindMeterHoldsQueuedOnlyWhenEnforced(t *testing.T) {
	for _, enforce := range []bool{true, false} {
		g := newRig(t, issueTask("3buqdlot", 7, "x"))
		g.r.Cfg.Budgets = config.Budgets{FactoryDaily: 25_000_000, HumanDaily: 5_000_000, EnforcePrincipal: enforce}
		if err := api.StampMeter(t.Context(), g.r.Ledger, "agent-system", now.Add(-time.Hour), 5*time.Minute); err != nil {
			t.Fatal(err)
		}
		tk := g.reconcile(t, "3buqdlot", 3)
		switch {
		case enforce && (tk.Status.Phase != v1alpha1.PhaseQueued || tk.Status.Reason != "waiting_meter_blind" || len(g.runs.runs) != 0):
			t.Fatalf("enforced: %s %s %v", tk.Status.Phase, tk.Status.Reason, g.runs.runs)
		case !enforce && tk.Status.Phase != v1alpha1.PhaseImplementing:
			t.Fatalf("shadow never holds: %s %s", tk.Status.Phase, tk.Status.Reason)
		}
	}
}
