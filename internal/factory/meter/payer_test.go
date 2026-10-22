// SPDX-License-Identifier: Apache-2.0

package meter

import (
	"context"
	"maps"
	"slices"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/fmetrics"
	"github.com/Smana/agent-platform/internal/factory/runs"
)

// tally is what OnCharge was told, in micro-USD by payer and kind.
type tally map[[2]string]int64

func (c tally) add(_ context.Context, principal, kind string, microUSD int64) {
	c[[2]string{principal, kind}] += microUSD
}

func (c tally) by(principal string) (n int64) {
	for k, v := range c {
		if k[0] == principal {
			n += v
		}
	}
	return n
}

func (c tally) total() (n int64) {
	for _, v := range c {
		n += v
	}
	return n
}

func sum(m map[string]int64) (n int64) {
	for _, v := range m {
		n += v
	}
	return n
}

// Spend per identity, offline proofs 1 and 2: runs paid by a labeller, by the fleet (a schedule's
// and RunLore's tasks), by a linked and an unlinked API caller, priced and not, ticked through a
// day. The payers' charges sum to what the ledger settled run by run, each run its total as the
// meter counts it; per person key they equal the day's spent, and over every payer the fleet's.
func TestThePayersChargesPartitionTheLedger(t *testing.T) {
	now := time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)
	run := func(id, task, room, principal, chargedTo string) runs.Run {
		return runs.Run{ID: id, TaskID: task, RoomRef: room, Phase: "Running", Model: "agent-default",
			Principal: principal, ChargedTo: chargedTo, Created: now}
	}
	st := &store{patches: map[string]map[string]string{}, runs: []runs.Run{
		run("aaaaaaaa", "3buqdlot", "3buqdlot", runs.PrincipalFactory, "github:alice"), // a labelled task: GLM, priced
		run("bbbbbbbb", "4buqdlot", "4buqdlot", runs.PrincipalFactory, ""),             // a schedule's task: priced
		run("cccccccc", "5buqdlot", "5buqdlot", runs.PrincipalFactory, ""),             // RunLore's task: untyped
		run("dddddddd", "", "6buqdlot", "human:291", "github:bob"),                     // a linked human's API run: Opus
		run("eeeeeeee", "", "", "human:292", "human:292"),                              // an unlinked one: a model with no price
	}}
	readings := []typed{
		{
			"aaaaaaaa": byModel(map[string]ModelUsage{"glm-5.3": {Input: 1_000_000, Output: 10_000}}),
			"bbbbbbbb": byModel(map[string]ModelUsage{"glm-5.3": {Input: 200_000, Output: 10_000}}),
			"cccccccc": {Tokens: 100_000},
			"dddddddd": byModel(map[string]ModelUsage{"claude-opus-5-5": {Input: 100_000, CacheRead: 90_000, Output: 1_000}}),
			"eeeeeeee": byModel(map[string]ModelUsage{"claude-fable-5-1": {Input: 9_000, Output: 1_000}}),
		},
		{
			"aaaaaaaa": byModel(map[string]ModelUsage{"glm-5.3": {Input: 1_500_000, Output: 20_000}}),
			"bbbbbbbb": byModel(map[string]ModelUsage{"glm-5.3": {Input: 300_000, Output: 10_000}}),
			"cccccccc": {Tokens: 150_000},
			"dddddddd": byModel(map[string]ModelUsage{"claude-opus-5-5": {Input: 200_000, CacheRead: 180_000, Output: 2_000}}),
			"eeeeeeee": byModel(map[string]ModelUsage{"claude-fable-5-1": {Input: 18_000, Output: 2_000}}),
		},
	}
	c := fake.NewClientBuilder().WithScheme(cmScheme()).Build()
	src := typed{}
	got := tally{}
	m := usdLedgerMeter(st, src, c, &now)
	m.OnCharge = got.add
	for i := range 3 {
		maps.Copy(src, readings[min(i, 1)])
		if i == 2 {
			st.runs[1].Phase, st.runs[1].Finished = "Succeeded", now // the schedule's run ended: nothing new
		}
		if err := m.Tick(t.Context()); err != nil {
			t.Fatalf("tick %d: %v", i, err)
		}
		now = now.Add(time.Hour)
	}
	// GLM: 1.5 M x 1.40 + 20 k x 4.40, and 300 k x 1.40 + 10 k x 4.40. Untyped: 150 k x 1.7. Opus:
	// 20 k fresh x 4 + 180 k read x 0.20 + 2 k x 20. Unpriced: 20 k tokens x 1.7.
	run1 := map[string]int64{"aaaaaaaa": 2_188_000, "bbbbbbbb": 464_000, "cccccccc": 255_000, "dddddddd": 156_000, "eeeeeeee": 34_000}
	settled := settledOf(t, c, "20261022")
	if !maps.Equal(settled, run1) {
		t.Fatalf("settled %v, want each run's total %v", settled, run1)
	}
	if got.total() != sum(settled) {
		t.Fatalf("the payers' charges sum to %d, the ledger settled %d", got.total(), sum(settled))
	}
	want := tally{{"github:alice", runs.KindTask}: 2_188_000, {runs.PrincipalFactory, runs.KindTask}: 719_000,
		{"github:bob", runs.KindRoom}: 156_000, {"human:292", runs.KindRoom}: 34_000}
	if !maps.Equal(got, want) {
		t.Fatalf("charges %v, want %v", got, want)
	}
	spent, _ := spentOf(t, c, "20261022")
	for _, person := range []string{"github:alice", "github:bob", "human:292"} {
		if got.by(person) != spent[person] {
			t.Errorf("%s: charged %d, the day spent %d", person, got.by(person), spent[person])
		}
	}
	if got.total() != spent[runs.PrincipalFactory] {
		t.Errorf("every payer's charges %d, the fleet's day %d", got.total(), spent[runs.PrincipalFactory])
	}
	if cost, _ := m.Costs.Get("aaaaaaaa"); got.by("github:alice") != cost.MicroUSD {
		t.Errorf("alice's run cost %d, charged to her %d: the task record's costMicroUSD lands under its chargedTo",
			cost.MicroUSD, got.by("github:alice"))
	}
}

// Offline proof 3: the counter follows the ledger through a leader change, a write in doubt that
// landed, a failed write, a run across midnight and the switch day's ended run.
func TestTheChargesFollowTheLedgerThroughItsFailures(t *testing.T) {
	at := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	t.Run("a leader change: two counters together", func(t *testing.T) {
		now := at
		c := fake.NewClientBuilder().WithScheme(cmScheme()).Build()
		first, next := tally{}, tally{}
		m := usdLedgerMeter(&store{patches: map[string]map[string]string{}, runs: []runs.Run{glmRun("aaaaaaaa", now)}},
			typed{"aaaaaaaa": glmUsage(1_000_000)}, c, &now)
		m.OnCharge = first.add
		if err := m.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
		r := glmRun("aaaaaaaa", now)
		r.Tokens = 1_000_000
		n := usdLedgerMeter(&store{patches: map[string]map[string]string{}, runs: []runs.Run{r}},
			typed{"aaaaaaaa": glmUsage(1_500_000)}, c, &now)
		n.OnCharge = next.add
		if err := n.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
		if s := settledOf(t, c, "20261022"); first.total()+next.total() != s["aaaaaaaa"] || first.total() != 1_400_000 {
			t.Fatalf("charged %d then %d, settled %d", first.total(), next.total(), s["aaaaaaaa"])
		}
	})
	for what, landed := range map[string]bool{"a failed write": false, "a write in doubt that landed": true} {
		t.Run(what, func(t *testing.T) {
			now := at
			fail := true
			c := flaky(&fail, &landed)
			got := tally{}
			src := typed{"aaaaaaaa": glmUsage(1_000_000)}
			m := usdLedgerMeter(&store{patches: map[string]map[string]string{}, runs: []runs.Run{glmRun("aaaaaaaa", now)}}, src, c, &now)
			m.OnCharge = got.add
			if err := m.Tick(t.Context()); err == nil {
				t.Fatal("the failed write is reported")
			}
			if got.total() != 0 {
				t.Fatalf("a write that reported an error counted %d", got.total())
			}
			fail, src["aaaaaaaa"] = false, glmUsage(1_100_000)
			if err := m.Tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			if s := settledOf(t, c, "20261022"); got.total() != s["aaaaaaaa"] || got.total() != 1_540_000 {
				t.Fatalf("charged %d, settled %d", got.total(), s["aaaaaaaa"])
			}
			// the run's payer is alice, not its principal: the resolve counts under hers
			if want := (tally{{"github:alice", runs.KindRoom}: 1_540_000}); !maps.Equal(got, want) {
				t.Fatalf("counted %v, want %v", got, want)
			}
		})
	}
	t.Run("a deleted run's write in doubt that landed is still counted", func(t *testing.T) {
		now := at
		fail, landed := true, true
		c := flaky(&fail, &landed)
		got := tally{}
		st := &store{patches: map[string]map[string]string{}, runs: []runs.Run{glmRun("aaaaaaaa", now)}}
		src := typed{"aaaaaaaa": glmUsage(1_000_000)}
		m := usdLedgerMeter(st, src, c, &now)
		m.OnCharge = got.add
		if err := m.Tick(t.Context()); err == nil {
			t.Fatal("the write in doubt is reported")
		}
		fail, st.runs = false, nil // the run is deleted
		m.Source = broken{}
		_ = m.Tick(t.Context()) // the usage read fails: forget runs, resolve does not
		m.Source = src
		if err := m.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
		spent, _ := spentOf(t, c, "20261022")
		if got.total() != spent[runs.PrincipalFactory] || got.total() == 0 {
			t.Fatalf("counted %d, spent %d", got.total(), spent[runs.PrincipalFactory])
		}
		if want := (tally{{"github:alice", runs.KindRoom}: got.total()}); !maps.Equal(got, want) {
			t.Fatalf("counted %v, want %v", got, want)
		}
	})
	t.Run("a run across midnight", func(t *testing.T) {
		now := time.Date(2026, 10, 22, 23, 59, 30, 0, time.UTC)
		c := fake.NewClientBuilder().WithScheme(cmScheme()).Build()
		got := tally{}
		src := typed{"aaaaaaaa": glmUsage(1_000_000)}
		m := usdLedgerMeter(&store{patches: map[string]map[string]string{}, runs: []runs.Run{glmRun("aaaaaaaa", now)}}, src, c, &now)
		m.OnCharge = got.add
		if err := m.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
		now, src["aaaaaaaa"] = now.Add(time.Minute), glmUsage(1_300_000)
		if err := m.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
		d1, d2 := settledOf(t, c, "20261022"), settledOf(t, c, "20261023")
		if got.total() != d1["aaaaaaaa"]+d2["aaaaaaaa"] || d2["aaaaaaaa"] != 420_000 {
			t.Fatalf("charged %d, the days settled %v and %v", got.total(), d1, d2)
		}
	})
	// R10: the switch day's ended run is charged nothing, and its late reading only that reading,
	// though the write that carries it also records the lift in the run's settled entry.
	for what, doubt := range map[string]bool{"written": false, "in doubt, landed": true} {
		t.Run("the switch day's ended run, then its late reading, "+what, func(t *testing.T) {
			now := at
			fail, landed := false, true
			c := flaky(&fail, &landed)
			got := tally{}
			r := glmRun("aaaaaaaa", now.Add(-time.Hour))
			r.Phase, r.Tokens = "Succeeded", 1_000_000 // the token ledgers charged it
			src := typed{"aaaaaaaa": glmUsage(1_000_000)}
			m := usdLedgerMeter(&store{patches: map[string]map[string]string{}, runs: []runs.Run{r}}, src, c, &now)
			m.OnCharge = got.add
			if err := m.Tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			if got.total() != 0 {
				t.Fatalf("nothing charged, nothing counted; counted %d", got.total())
			}
			src["aaaaaaaa"], fail = glmUsage(1_050_000), doubt // a late reading: 70 k more
			if err := m.Tick(t.Context()); (err != nil) != doubt {
				t.Fatalf("the late reading's tick: %v", err)
			}
			fail = false
			if doubt {
				if err := m.Tick(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			spent, _ := spentOf(t, c, "20261022")
			if got.total() != 70_000 || spent[runs.PrincipalFactory] != 70_000 || settledOf(t, c, "20261022")["aaaaaaaa"] != 1_470_000 {
				t.Fatalf("counted %d, spent %v, settled %v: the counter follows spent, not the lift",
					got.total(), spent, settledOf(t, c, "20261022"))
			}
			if want := (tally{{"github:alice", runs.KindRoom}: 70_000}); !maps.Equal(got, want) {
				t.Fatalf("counted %v, want %v", got, want)
			}
		})
	}
}

// Offline proof 4: the charge info names every live run, and an ended one until endSettle after
// its finishedAt; never one a new leader first sees ended longer ago, nor one with no finishedAt.
// It follows the listed runs, so a failed usage read does not freeze it.
func TestTheChargeInfoNamesLiveRunsAndThoseJustEnded(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	live := glmRun("aaaaaaaa", now)
	ended := glmRun("bbbbbbbb", now)
	ended.Phase, ended.Finished = "Succeeded", now.Add(-time.Minute)
	old := glmRun("cccccccc", now)
	old.Phase, old.Finished = "Failed", now.Add(-5*time.Minute)
	bare := glmRun("dddddddd", now)
	bare.Phase = "Succeeded"
	var named []string
	m := usdLedgerMeter(&store{patches: map[string]map[string]string{}, runs: []runs.Run{live, ended, old, bare}}, typed{},
		fake.NewClientBuilder().WithScheme(cmScheme()).Build(), &now)
	m.OnCharges = func(rs []runs.Run) {
		named = nil
		for _, r := range rs {
			named = append(named, r.ID)
		}
		slices.Sort(named)
	}
	if err := m.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(named, []string{"aaaaaaaa", "bbbbbbbb"}) {
		t.Fatalf("named %v, want the live run and the one ended a minute ago", named)
	}
	now = now.Add(90 * time.Second)
	m.Source = broken{}
	if err := m.Tick(t.Context()); err == nil {
		t.Fatal("the failed read is reported")
	}
	if !slices.Equal(named, []string{"aaaaaaaa"}) {
		t.Fatalf("named %v, want the live run alone: bbbbbbbb ended 2.5 minutes ago", named)
	}
}

// SI4: a revocation the meter writes is recorded under the run's payer.
func TestARevocationNamesTheRunsPayer(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	st := &store{patches: map[string]map[string]string{}, runs: []runs.Run{
		{ID: "aaaaaaaa", Phase: "Running", MaxTokens: 100, Principal: runs.PrincipalFactory, ChargedTo: "github:alice", Created: now}}}
	var who, why string
	m := &Meter{Runs: st, Source: source{"aaaaaaaa": 200}, Budgets: config.Budgets{}, Now: func() time.Time { return now },
		OnRevoke: func(ctx context.Context, reason, _ string) { who, why = fmetrics.PrincipalOf(ctx), reason }}
	if err := m.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if who != "github:alice" || why != ReasonBudgetRun {
		t.Fatalf("revoked %q under %q, want budget-run under github:alice", why, who)
	}
}

// SI3: each hand-over is a slice of its own, since the charge info keeps it until the next: a
// later tick never rewrites the runs an earlier one named.
func TestEachTickHandsOverItsOwnChargeInfo(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	st := &store{patches: map[string]map[string]string{}}
	var handed [][]runs.Run
	m := usdLedgerMeter(st, typed{}, fake.NewClientBuilder().WithScheme(cmScheme()).Build(), &now)
	m.OnCharges = func(rs []runs.Run) { handed = append(handed, rs) }
	for _, id := range []string{"aaaaaaaa", "bbbbbbbb"} {
		st.runs = []runs.Run{glmRun(id, now)}
		if err := m.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if len(handed) != 2 || len(handed[0]) != 1 || handed[0][0].ID != "aaaaaaaa" || handed[1][0].ID != "bbbbbbbb" {
		t.Fatalf("handed %v: the first tick's runs were rewritten", handed)
	}
}

// SI3: the charge info is set by the leader. Start runs only while the meter leads, and hands over
// no run as it returns, so the series end with the leadership.
func TestAMeterThatStopsLeadingNamesNoRun(t *testing.T) {
	st := &store{patches: map[string]map[string]string{}, runs: []runs.Run{{ID: "aaaaaaaa", Phase: "Running"}}}
	handed := make(chan []runs.Run, 4)
	m := &Meter{Runs: st, Source: source{"aaaaaaaa": 1}, Every: time.Second, OnCharges: func(rs []runs.Run) { handed <- rs },
		Ticker: func(time.Duration) (<-chan time.Time, func()) { return make(chan time.Time), func() {} }}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error)
	go func() { done <- m.Start(ctx) }()
	select {
	case rs := <-handed:
		if len(rs) != 1 {
			t.Fatalf("the leader named %v, want its live run", rs)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the first tick named no run")
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	select {
	case rs := <-handed:
		if len(rs) != 0 {
			t.Fatalf("a meter no longer leading named %v", rs)
		}
	default:
		t.Fatal("a meter no longer leading left the leader's runs named")
	}
}

// SI1: a run's payer is the last ledger key it settles to, so the payers' charges partition the
// fleet's and each person's are that person's day.
func TestARunsPayerIsTheLastKeyItSettlesTo(t *testing.T) {
	for name, r := range map[string]runs.Run{
		"a labelled task's run":    {TaskID: "3buqdlot", Principal: runs.PrincipalFactory, ChargedTo: "github:alice"},
		"a schedule's run":         {TaskID: "4buqdlot", Principal: runs.PrincipalFactory},
		"a linked human's API run": {Principal: "human:291", ChargedTo: "github:bob"},
		"an unlinked one":          {Principal: "human:292", ChargedTo: "human:292"},
		"one without chargedTo":    {Principal: "human:293"},
	} {
		if keys := charged(r); keys[len(keys)-1] != r.Payer() {
			t.Errorf("%s: settles to %v, its payer is %q", name, keys, r.Payer())
		}
	}
}
