// SPDX-License-Identifier: Apache-2.0

package meter

import (
	"context"
	"maps"
	"testing"
	"time"

	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/runs"
)

type reported struct {
	tier    string
	usd     float64
	byModel map[string]float64
}

// xplane-run-nh5tztlh (aws-0, 2026-10-16): the gateway's last scrape of the run landed after the
// meter's first tick that saw it Succeeded, so its cost was reported at 370,526 of its 507,350
// tokens ($1.54 of $2.13). An ended run's cost is reported once its late readings had their
// window, at the reading that window brought.
func TestAnEndedRunsCostWaitsForItsLateReadings(t *testing.T) {
	clock := time.Date(2026, 10, 16, 15, 53, 15, 0, time.UTC)
	r := standardRun("nh5tztlh")
	r.MaxTokens = 0
	st := &store{patches: map[string]map[string]string{}, runs: []runs.Run{r}}
	opus := func(in, out int64) Usage {
		return byModel(map[string]ModelUsage{"claude-opus-5-5": {Input: in, Output: out}})
	}
	src := typed{"nh5tztlh": opus(366_751, 3_775)}
	var got []reported
	m := &Meter{Runs: st, Source: src, Prices: served, Now: func() time.Time { return clock },
		OnCost: func(_ context.Context, tier string, usd float64, byModel map[string]float64) {
			got = append(got, reported{tier, usd, byModel})
		}}
	tick := func() {
		t.Helper()
		if err := m.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
		clock = clock.Add(30 * time.Second)
	}
	tick()                         // live, $1.542504
	st.runs[0].Phase = "Succeeded" // the pod ended; its last calls are not scraped yet
	tick()
	if len(got) != 0 {
		t.Fatalf("reported %v at the first ended tick, before the late readings", got)
	}
	src["nh5tztlh"] = opus(500_930, 6_420) // the last scrape lands
	for range 4 {
		tick()
	}
	if len(got) != 1 || got[0].usd != 2.13212 || got[0].tier != "standard" {
		t.Fatalf("reported %+v, want one $2.13212", got)
	}
	if want := map[string]float64{"claude-opus-5-5": 2.13212}; !maps.Equal(got[0].byModel, want) {
		t.Errorf("by model %v, want %v", got[0].byModel, want)
	}
	if n := st.patches["nh5tztlh"][runs.AnnUsage]; n != "507350" {
		t.Errorf("usage annotation %q, want 507350", n)
	}
}

// A run deleted before its window closes (a stop, the kill switch) still reports what it spent.
func TestADeletedRunReportsItsCost(t *testing.T) {
	st := &store{patches: map[string]map[string]string{}, runs: []runs.Run{standardRun("aaaaaaaa")}}
	src := typed{"aaaaaaaa": byModel(map[string]ModelUsage{"glm-5.3": {Input: 1_000_000}})}
	var got []reported
	m := &Meter{Runs: st, Source: src, Prices: served,
		OnCost: func(_ context.Context, tier string, usd float64, byModel map[string]float64) {
			got = append(got, reported{tier, usd, byModel})
		}}
	if err := m.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	st.runs = nil
	for range 2 {
		if err := m.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if len(got) != 1 || got[0].usd != 1.4 || got[0].byModel["glm-5.3"] != 1.4 {
		t.Fatalf("reported %+v, want one $1.40 on glm-5.3", got)
	}
}

// The live spend is each unreported priced run's cost by the model that served it; the claim's
// model names the tokens no model label does. A reported run leaves the snapshot.
func TestTheLiveSpendSnapshot(t *testing.T) {
	clock := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	st := &store{patches: map[string]map[string]string{}, runs: []runs.Run{standardRun("aaaaaaaa")}}
	u := byModel(map[string]ModelUsage{"claude-opus-5-5": {Input: 100_000}, "": {Input: 1_000_000}})
	var snap map[string]map[string]float64
	m := &Meter{Runs: st, Source: typed{"aaaaaaaa": u}, Now: func() time.Time { return clock },
		Prices:  map[string]config.Price{"claude-opus-5-5": served["claude-opus-5-5"], "agent-default": glm["agent-default"]},
		OnSpend: func(s map[string]map[string]float64) { snap = s }}
	if err := m.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{"claude-opus-5-5": 0.4, "agent-default": 1.4}
	if len(snap) != 1 || !maps.Equal(snap["aaaaaaaa"], want) {
		t.Fatalf("snapshot %v, want %v", snap, want)
	}
	st.runs[0].Phase = "Succeeded"
	for range 6 {
		clock = clock.Add(30 * time.Second)
		if err := m.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if len(snap) != 0 {
		t.Fatalf("snapshot %v after the run was reported, want empty", snap)
	}
}

// A reading that lands after the report is not lost: the late spend goes to OnLateCost, by the
// model that served it, while the run's claim exists.
func TestSpendAfterTheReportIsAddedLate(t *testing.T) {
	clock := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	st := &store{patches: map[string]map[string]string{}, runs: []runs.Run{standardRun("aaaaaaaa")}}
	src := typed{"aaaaaaaa": byModel(map[string]ModelUsage{"glm-5.3": {Input: 1_000_000}})}
	var costs []float64
	var late []map[string]float64
	m := &Meter{Runs: st, Source: src, Prices: served, Now: func() time.Time { return clock },
		OnCost:     func(_ context.Context, _ string, usd float64, _ map[string]float64) { costs = append(costs, usd) },
		OnLateCost: func(_ context.Context, byModel map[string]float64) { late = append(late, byModel) }}
	tick := func() {
		t.Helper()
		if err := m.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
		clock = clock.Add(30 * time.Second)
	}
	tick()
	st.runs[0].Phase = "Succeeded"
	for range 6 {
		tick()
	}
	src["aaaaaaaa"] = byModel(map[string]ModelUsage{"glm-5.3": {Input: 1_500_000}})
	tick()
	tick()
	if len(costs) != 1 || costs[0] != 1.4 {
		t.Fatalf("costs %v, want one $1.40", costs)
	}
	if len(late) != 1 || !maps.Equal(late[0], map[string]float64{"glm-5.3": 0.7}) {
		t.Fatalf("late %v, want one $0.70 on glm-5.3", late)
	}
}
