// SPDX-License-Identifier: Apache-2.0

package meter

import (
	"context"
	"errors"
	"maps"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Smana/agent-platform/internal/factory/api"
	"github.com/Smana/agent-platform/internal/factory/runs"
)

// settledOf reads a day ledger's settled.<runId> entries back.
func settledOf(t *testing.T, c client.Client, day string) map[string]int64 {
	t.Helper()
	var cm corev1.ConfigMap
	if err := c.Get(t.Context(), types.NamespacedName{Namespace: "agent-system", Name: api.LedgerPrefix + day}, &cm); err != nil {
		t.Fatal(err)
	}
	s, err := api.SettledColumn(&cm)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// glmRun is a factory run served by GLM-5.3, charged to alice beside the fleet.
func glmRun(id string, at time.Time) runs.Run {
	return runs.Run{ID: id, Phase: "Running", Tier: "standard", Model: "agent-default", MaxTokens: 1_500_000,
		Principal: runs.PrincipalFactory, ChargedTo: "github:alice", Created: at}
}

// glmUsage is input fresh tokens on GLM-5.3: 1.4 micro-USD each.
func glmUsage(input int64) Usage {
	return byModel(map[string]ModelUsage{"glm-5.3": {Input: input}})
}

// usdLedgerMeter meters st from src against c at *now, priced by the served models.
func usdLedgerMeter(st Store, src Source, c client.Client, now *time.Time) *Meter {
	return &Meter{Runs: st, Source: src, Prices: served, Ledger: c, Namespace: "agent-system", Costs: &Costs{},
		Now: func() time.Time { return *now }}
}

// TC6/TC7: a tick charges a run's priced increase to every key it is charged to and to its own
// settled entry, in one write of the day's ledger.
func TestATickChargesBothKeysAndTheRunInOneWrite(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	writes := 0
	c := fake.NewClientBuilder().WithScheme(cmScheme()).WithInterceptorFuncs(interceptor.Funcs{
		Create: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if obj.GetName() == api.LedgerPrefix+"20261022" {
				writes++
			}
			return cl.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if obj.GetName() == api.LedgerPrefix+"20261022" {
				writes++
			}
			return cl.Update(ctx, obj, opts...)
		},
	}).Build()
	st := &store{patches: map[string]map[string]string{}, runs: []runs.Run{glmRun("aaaaaaaa", now)}}
	src := typed{"aaaaaaaa": byModel(map[string]ModelUsage{"glm-5.3": {Input: 1_000_000, Output: 10_000}})}
	if err := usdLedgerMeter(st, src, c, &now).Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	// 1,000,000 x 1.40 + 10,000 x 4.40 = 1,444,000 micro-USD
	spent, _ := spentOf(t, c, "20261022")
	if want := map[string]int64{runs.PrincipalFactory: 1_444_000, "github:alice": 1_444_000}; !maps.Equal(spent, want) {
		t.Fatalf("spent %v, want %v", spent, want)
	}
	if s := settledOf(t, c, "20261022"); !maps.Equal(s, map[string]int64{"aaaaaaaa": 1_444_000}) {
		t.Fatalf("settled %v", s)
	}
	if writes != 1 {
		t.Fatalf("%d ledger writes, want one", writes)
	}
}

// TC6: a second meter taking over mid-run starts the run at what the ledgers settled for it, or at
// its counters when they are higher: it charges neither twice nor less, with or without a reset
// of the gateway's counters between the two leaders.
func TestANewLeaderChargesNeitherTwiceNorLess(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		after []int64 // the new leader's readings of the run's input
		want  int64   // what the day holds for the run
	}{
		"no counter reset": {[]int64{1_500_000}, 2_100_000},
		// The gateway restarted: the new leader's first reading is below what was settled, and
		// only what the run spends after it is charged on top.
		"a counter reset": {[]int64{200_000, 500_000}, 1_820_000},
	} {
		t.Run(name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(cmScheme()).Build()
			first := &store{patches: map[string]map[string]string{}, runs: []runs.Run{glmRun("aaaaaaaa", now)}}
			if err := usdLedgerMeter(first, typed{"aaaaaaaa": glmUsage(1_000_000)}, c, &now).Tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			run := glmRun("aaaaaaaa", now)
			run.Tokens = 1_000_000 // what the first leader annotated
			src := typed{}
			next := usdLedgerMeter(&store{patches: map[string]map[string]string{}, runs: []runs.Run{run}}, src, c, &now)
			for _, in := range tc.after {
				src["aaaaaaaa"] = glmUsage(in)
				if err := next.Tick(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			spent, _ := spentOf(t, c, "20261022")
			if spent[runs.PrincipalFactory] != tc.want || spent["github:alice"] != tc.want {
				t.Fatalf("spent %v, want %d on both keys", spent, tc.want)
			}
			if s := settledOf(t, c, "20261022"); s["aaaaaaaa"] != tc.want {
				t.Fatalf("settled %v, want %d", s, tc.want)
			}
			if got, _ := next.Costs.Get("aaaaaaaa"); got.MicroUSD != tc.want {
				t.Fatalf("the run's cost %+v, want %d: its cap reads it", got, tc.want)
			}
		})
	}
}

// R07 and TC6: a run across midnight charges each day only what it spent in it, and a new leader
// the next day reads what every day settled for it.
func TestARunAcrossMidnightChargesEachDayItsOwnIncrease(t *testing.T) {
	now := time.Date(2026, 10, 22, 23, 59, 30, 0, time.UTC)
	c := fake.NewClientBuilder().WithScheme(cmScheme()).Build()
	st := &store{patches: map[string]map[string]string{}, runs: []runs.Run{glmRun("aaaaaaaa", now)}}
	src := typed{"aaaaaaaa": glmUsage(1_000_000)}
	m := usdLedgerMeter(st, src, c, &now)
	if err := m.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute) // 2026-10-23 00:00:30
	src["aaaaaaaa"] = glmUsage(1_200_000)
	if err := m.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if spent, _ := spentOf(t, c, "20261022"); spent[runs.PrincipalFactory] != 1_400_000 {
		t.Fatalf("the first day: %v", spent)
	}
	if spent, _ := spentOf(t, c, "20261023"); spent[runs.PrincipalFactory] != 280_000 {
		t.Fatalf("the second day holds its own increase only: %v", spent)
	}
	now = now.Add(30 * time.Second)
	src["aaaaaaaa"] = glmUsage(1_300_000)
	next := usdLedgerMeter(&store{patches: map[string]map[string]string{}, runs: []runs.Run{glmRun("aaaaaaaa", now)}}, src, c, &now)
	if err := next.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if spent, _ := spentOf(t, c, "20261023"); spent[runs.PrincipalFactory] != 420_000 {
		t.Fatalf("a new leader charges the run's 1.82 M micro-USD less both days' 1.68 M: %v", spent)
	}
	if s := settledOf(t, c, "20261023"); s["aaaaaaaa"] != 420_000 {
		t.Fatalf("settled %v", s)
	}
}

// TC7: a run the meter cannot price is charged at the reference rate, 1.7 micro-USD a token,
// never $0: an untyped reading, and a model with no price.
func TestARunTheMeterCannotPriceIsChargedTheReferenceRate(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	for name, u := range map[string]Usage{
		"untyped":  {Tokens: 1000},
		"unpriced": byModel(map[string]ModelUsage{"claude-fable-5-1": {Input: 900, Output: 100}}),
	} {
		t.Run(name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(cmScheme()).Build()
			st := &store{patches: map[string]map[string]string{}, runs: []runs.Run{glmRun("aaaaaaaa", now)}}
			if err := usdLedgerMeter(st, typed{"aaaaaaaa": u}, c, &now).Tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			if spent, _ := spentOf(t, c, "20261022"); spent[runs.PrincipalFactory] != 1700 {
				t.Fatalf("spent %v, want 1700", spent)
			}
		})
	}
}

// TC9: the switch day starts at $0. A run in flight has nothing settled, so its whole cost is
// charged; a run that ended before the switch, annotated by the token ledgers' meter, is not
// charged again; a run that ended unannotated, between two leaders, is charged.
func TestTheSwitchDayStartsAtZero(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	c := fake.NewClientBuilder().WithScheme(cmScheme()).Build()
	st := &store{patches: map[string]map[string]string{}, runs: []runs.Run{
		{ID: "aaaaaaaa", Phase: "Running", Tokens: 1000, Principal: "human:1", Created: now},
		{ID: "bbbbbbbb", Phase: "Succeeded", Tokens: 5_000_000, Principal: "human:2", Created: now.Add(-time.Hour)},
		{ID: "cccccccc", Phase: "Failed", Principal: "human:3", Created: now.Add(-time.Hour)},
	}}
	src := source{"aaaaaaaa": 1200, "bbbbbbbb": 5_000_000, "cccccccc": 300}
	if err := (&Meter{Runs: st, Source: src, Ledger: c, Namespace: "agent-system", Now: func() time.Time { return now }}).Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	spent, _ := spentOf(t, c, "20261022")
	if want := map[string]int64{"human:1": 2040, "human:3": 510}; !maps.Equal(spent, want) {
		t.Fatalf("spent %v, want %v", spent, want)
	}
}

// TC6: a meter that cannot read what the ledgers settled charges nothing, annotates nothing and
// stamps nothing; once it can, the run is charged once.
func TestAnUnreadLedgerChargesNothingUntilItIsRead(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	down := true
	c := fake.NewClientBuilder().WithScheme(cmScheme()).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, cl client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if down {
				return errors.New("apiserver down")
			}
			return cl.List(ctx, list, opts...)
		},
	}).Build()
	st := &store{patches: map[string]map[string]string{}, runs: []runs.Run{glmRun("aaaaaaaa", now)}}
	m := usdLedgerMeter(st, typed{"aaaaaaaa": glmUsage(1_000_000)}, c, &now)
	var stamped bool
	m.OnSucceeded = func(context.Context, time.Time) { stamped = true }
	if err := m.Tick(t.Context()); err == nil {
		t.Fatal("an unread ledger is reported")
	}
	if _, found := spentOf(t, c, "20261022"); found || stamped || st.patches["aaaaaaaa"][runs.AnnUsage] != "" {
		t.Fatalf("charged %v, stamped %v, annotated %v", found, stamped, st.patches)
	}
	down = false
	for range 2 {
		if err := m.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if spent, _ := spentOf(t, c, "20261022"); spent[runs.PrincipalFactory] != 1_400_000 || !stamped {
		t.Fatalf("spent %v, stamped %v: want the run's 1.4 M micro-USD once", spent, stamped)
	}
}

// TC6: on a day whose ledger already exists, the usual case, the Update that carries the spent
// column carries the run's settled entry too: one write, never two a failure could split.
func TestAnExistingDaysChargeAndSettledAreOneUpdate(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	var writes []*corev1.ConfigMap
	c := fake.NewClientBuilder().WithScheme(cmScheme()).
		WithObjects(ledgerDay(t, "20261022", map[string]int64{"github:bob": 5}, nil)).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
				if cm, ok := obj.(*corev1.ConfigMap); ok && cm.Name == api.LedgerPrefix+"20261022" {
					writes = append(writes, cm.DeepCopy())
				}
				return cl.Update(ctx, obj, opts...)
			},
		}).Build()
	st := &store{patches: map[string]map[string]string{}, runs: []runs.Run{glmRun("aaaaaaaa", now)}}
	if err := usdLedgerMeter(st, typed{"aaaaaaaa": glmUsage(1_000_000)}, c, &now).Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(writes) != 1 {
		t.Fatalf("%d ledger updates, want one", len(writes))
	}
	spent, err := api.LedgerSpentColumn(writes[0])
	if err != nil {
		t.Fatal(err)
	}
	settled, err := api.SettledColumn(writes[0])
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]int64{runs.PrincipalFactory: 1_400_000, "github:alice": 1_400_000, "github:bob": 5}; !maps.Equal(spent, want) {
		t.Fatalf("the update's spent %v, want %v", spent, want)
	}
	if !maps.Equal(settled, map[string]int64{"aaaaaaaa": 1_400_000}) {
		t.Fatalf("the update's settled %v", settled)
	}
}

// R9: a new leader's lift of a run's USD total to what was settled holds on a tick with no
// reading too: the run's cap and its cost read the lifted total, never the reset counters.
func TestALiftedCostHoldsWithoutAReading(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	c := fake.NewClientBuilder().WithScheme(cmScheme()).WithObjects(settledIn(t,
		ledgerDay(t, "20261022", map[string]int64{runs.PrincipalFactory: 1_400_000, "github:alice": 1_400_000}, nil),
		map[string]int64{"aaaaaaaa": 1_400_000})).Build()
	run := glmRun("aaaaaaaa", now)
	run.Tokens = 1_000_000
	src := typed{"aaaaaaaa": glmUsage(200_000)} // the gateway restarted: 280,000 micro-USD on its counters
	m := usdLedgerMeter(&store{patches: map[string]map[string]string{}, runs: []runs.Run{run}}, src, c, &now)
	for _, reading := range []bool{true, false} {
		if !reading {
			delete(src, "aaaaaaaa")
		}
		if err := m.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got, _ := m.Costs.Get("aaaaaaaa"); got.MicroUSD != 1_400_000 {
			t.Fatalf("with a reading %v: the run's cost %+v, want the settled 1,400,000", reading, got)
		}
	}
}
