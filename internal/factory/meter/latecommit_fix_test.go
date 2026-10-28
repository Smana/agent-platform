// SPDX-License-Identifier: Apache-2.0

package meter

import (
	"strings"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Smana/maku/internal/factory/api"
	"github.com/Smana/maku/internal/factory/runs"
)

var (
	day22    = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	midnight = time.Date(2026, 10, 22, 23, 59, 30, 0, time.UTC)
)

// totals is what the two days' ledgers hold for the factory key, and each run's settled entries
// summed.
func totals(t *testing.T, c client.Client) (spent int64, settled map[string]int64) {
	t.Helper()
	settled = map[string]int64{}
	for _, d := range []string{"20261022", "20261023"} {
		if s, ok := spentOf(t, c, d); ok {
			spent += s[runs.PrincipalFactory]
			for id, n := range settledOf(t, c, d) {
				settled[id] += n
			}
		}
	}
	return spent, settled
}

// #166 across a failover: leader A's write answers 504 and commits only after leader B read the
// ledgers, so B holds no entry for it. B nets against what it learned, as A would have: the run's
// 1.4 M is charged once, however the late write falls and on whichever day it lands.
func TestANewLeaderChargesAnOldLeadersLateCommitOnce(t *testing.T) {
	for name, tc := range map[string]struct {
		start    time.Time
		at       int  // the ledger read of B's tick that finds the write committed; 0: none
		atUpdate bool // the write commits between B's read and its Update
		after    bool // the write commits after B's tick: it must conflict
	}{
		"before B reads the day":            {day22, 1, false, false},
		"between B's read and its Update":   {day22, 0, true, false},
		"after B's write":                   {day22, 0, false, true},
		"across midnight, before the fence": {midnight, 1, false, false},
		"across midnight, after the fence":  {midnight, 0, false, true},
	} {
		t.Run(name, func(t *testing.T) {
			now := tc.start
			day := now.Format(api.LedgerDayLayout)
			c := newLateCommit(ledgerDay(t, day, map[string]int64{"github:bob": 5}, nil))
			src := typed{"aaaaaaaa": glmUsage(1_000_000)}
			run := glmRun("aaaaaaaa", now)
			a := usdLedgerMeter(&store{patches: map[string]map[string]string{}, runs: []runs.Run{run}}, src, c, &now)
			c.failing = true
			if err := a.Tick(t.Context()); err == nil || c.held == nil {
				t.Fatalf("leader A: the 504 is reported and the write held: %v", err)
			}
			c.failing, c.ticking, c.at, c.atUpdate = false, 0, tc.at, tc.atUpdate
			now = now.Add(time.Minute)
			b := usdLedgerMeter(&store{patches: map[string]map[string]string{}, runs: []runs.Run{run}}, src, c, &now)
			got := tally{}
			b.OnCharge = got.add
			if err := b.Tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			if tc.after {
				c.commitHeld(t.Context(), c.WithWatch)
				if !apierrors.IsConflict(c.commit) && !apierrors.IsAlreadyExists(c.commit) {
					t.Fatalf("A's write after B's: %v, want a conflict", c.commit)
				}
			} else if c.commit != nil || c.held != nil {
				t.Fatalf("A's write: %v, held %v", c.commit, c.held != nil)
			}
			spent, settled := totals(t, c)
			if spent != 1_400_000 || settled["aaaaaaaa"] != 1_400_000 {
				t.Fatalf("spent %d, settled %d, want the run's 1400000 once", spent, settled["aaaaaaaa"])
			}
		})
	}
}

// The default error branch: a tick whose own write also reports an error, after the earlier write
// committed meanwhile, counts that landed write at once and leaves only its own in doubt. The
// run is then charged 1.54 M once: the first write's 1.4 M and the second's 0.14 M.
func TestALateCommitThenAnotherLostWriteIsChargedOnce(t *testing.T) {
	now := day22
	c := newLateCommit(ledgerDay(t, "20261022", map[string]int64{"github:bob": 5}, nil))
	src := typed{"aaaaaaaa": glmUsage(1_000_000)}
	m := usdLedgerMeter(&store{patches: map[string]map[string]string{}, runs: []runs.Run{glmRun("aaaaaaaa", now)}}, src, c, &now)
	got := tally{}
	m.OnCharge = got.add
	c.failing = true
	if err := m.Tick(t.Context()); err == nil || c.held == nil {
		t.Fatalf("tick N: %v", err)
	}
	// Tick N+1: the first write commits at settle's read, and this tick's own write is lost too.
	c.ticking, c.at = 0, 2
	now, src["aaaaaaaa"] = now.Add(time.Minute), glmUsage(1_100_000)
	if err := m.Tick(t.Context()); err == nil || c.held == nil {
		t.Fatalf("tick N+1: the second 504 is reported and the write held: %v", err)
	}
	if got.total() != 1_400_000 {
		t.Fatalf("the landed write counted %d, want 1400000 at once", got.total())
	}
	// Tick N+2: the second write commits at settle's read.
	c.failing, c.ticking, c.at = false, 0, 2
	now = now.Add(time.Minute)
	if err := m.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	spent, settled := totals(t, c)
	if spent != 1_540_000 || settled["aaaaaaaa"] != 1_540_000 || got.total() != 1_540_000 {
		t.Fatalf("spent %d, settled %d, counted %d, want 1540000 once", spent, settled["aaaaaaaa"], got.total())
	}
}

// A late write that commits between settle's read and its Update: the Update conflicts, and the
// retry nets the write it now finds.
func TestALateCommitBetweenSettlesReadAndUpdateIsChargedOnce(t *testing.T) {
	now := day22
	c := newLateCommit(ledgerDay(t, "20261022", map[string]int64{"github:bob": 5}, nil))
	m := usdLedgerMeter(&store{patches: map[string]map[string]string{}, runs: []runs.Run{glmRun("aaaaaaaa", now)}},
		typed{"aaaaaaaa": glmUsage(1_000_000)}, c, &now)
	c.failing = true
	if err := m.Tick(t.Context()); err == nil || c.held == nil {
		t.Fatalf("tick N: %v", err)
	}
	c.failing, c.atUpdate = false, true
	now = now.Add(time.Minute)
	if err := m.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if c.commit != nil {
		t.Fatalf("the late write: %v", c.commit)
	}
	if spent, settled := totals(t, c); spent != 1_400_000 || settled["aaaaaaaa"] != 1_400_000 {
		t.Fatalf("spent %d, settled %d, want 1400000 once", spent, settled["aaaaaaaa"])
	}
}

// fence's branches, across midnight: its conflict retry, a ledger the lost write would have
// created, and a day pruned since.
func TestFenceRetriesCreatesAndLetsAPrunedDayGo(t *testing.T) {
	onRun := func(c *lateCommit, now *time.Time) *Meter {
		return usdLedgerMeter(&store{patches: map[string]map[string]string{}, runs: []runs.Run{glmRun("aaaaaaaa", *now)}},
			typed{"aaaaaaaa": glmUsage(1_000_000)}, c, now)
	}
	t.Run("a conflict is retried", func(t *testing.T) {
		now := midnight
		c := newLateCommit(ledgerDay(t, "20261022", map[string]int64{"github:bob": 5}, nil))
		m := onRun(c, &now)
		c.failing = true
		if err := m.Tick(t.Context()); err == nil || c.held == nil {
			t.Fatalf("tick N: %v", err)
		}
		c.failing, c.ticking, c.at = false, 0, 2 // the late write commits at the fence's retry read
		c.conflicts = map[string]int{api.LedgerPrefix + "20261022": 1}
		now = now.Add(time.Minute)
		if err := m.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
		if n := c.updates[api.LedgerPrefix+"20261022"]; n < 3 { // the 504, the conflict, the fence's retry
			t.Fatalf("%d Updates of the 22nd, want the fence to retry", n)
		}
		if spent, settled := totals(t, c); spent != 1_400_000 || settled["aaaaaaaa"] != 1_400_000 {
			t.Fatalf("spent %d, settled %d, want 1400000 once", spent, settled["aaaaaaaa"])
		}
	})
	t.Run("a day that stays busy fails the tick, charging nothing", func(t *testing.T) {
		now := midnight
		c := newLateCommit(ledgerDay(t, "20261022", map[string]int64{"github:bob": 5}, nil))
		m := onRun(c, &now)
		c.failing = true
		if err := m.Tick(t.Context()); err == nil {
			t.Fatal("tick N: want the 504")
		}
		c.failing = false
		c.conflicts = map[string]int{api.LedgerPrefix + "20261022": api.LedgerAttempts}
		now = now.Add(time.Minute)
		if err := m.Tick(t.Context()); err == nil || !strings.Contains(err.Error(), "stayed busy") {
			t.Fatalf("want the busy day to fail the tick: %v", err)
		}
		if _, ok := spentOf(t, c, "20261023"); ok {
			t.Fatal("a tick that could not fence wrote the next day")
		}
	})
	t.Run("a ledger the lost write would have created", func(t *testing.T) {
		for _, before := range []bool{false, true} {
			now := midnight
			c := newLateCommit() // the 22nd's ledger does not exist: the lost write is a Create
			m := onRun(c, &now)
			c.failing = true
			if err := m.Tick(t.Context()); err == nil || c.held == nil {
				t.Fatalf("tick N: %v", err)
			}
			c.failing, c.ticking, c.at = false, 0, 0
			if before {
				c.at = 1 // the Create commits at the fence's read
			}
			now = now.Add(time.Minute)
			if err := m.Tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			if !before {
				c.commitHeld(t.Context(), c.WithWatch)
				if !apierrors.IsAlreadyExists(c.commit) {
					t.Fatalf("the late Create after the fence: %v, want AlreadyExists", c.commit)
				}
			}
			if spent, settled := totals(t, c); spent != 1_400_000 || settled["aaaaaaaa"] != 1_400_000 {
				t.Fatalf("before=%v: spent %d, settled %d, want 1400000 once", before, spent, settled["aaaaaaaa"])
			}
		}
	})
	t.Run("a pruned day is let go", func(t *testing.T) {
		now := midnight
		c := newLateCommit()
		m := onRun(c, &now)
		c.failing = true
		if err := m.Tick(t.Context()); err == nil || c.held == nil {
			t.Fatalf("tick N: %v", err)
		}
		c.failing = false
		now = now.Add(40 * 24 * time.Hour)
		if err := m.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, ok := spentOf(t, c, "20261022"); ok {
			t.Fatal("the fence created a ledger the pruner would delete")
		}
		if s, ok := spentOf(t, c, now.Format(api.LedgerDayLayout)); !ok || s[runs.PrincipalFactory] != 1_400_000 {
			t.Fatalf("today's ledger holds %v, want the run's 1400000", s)
		}
	})
}

// Two runs in doubt that share ledger keys, both landing late: each is netted off the shared
// key, so the key holds each run's charge once.
func TestTwoRunsSharingAKeyAreEachChargedOnceAfterALateCommit(t *testing.T) {
	now := day22
	c := newLateCommit(ledgerDay(t, "20261022", map[string]int64{"github:bob": 5}, nil))
	st := &store{patches: map[string]map[string]string{}, runs: []runs.Run{glmRun("aaaaaaaa", now), glmRun("bbbbbbbb", now)}}
	src := typed{"aaaaaaaa": glmUsage(1_000_000), "bbbbbbbb": glmUsage(500_000)}
	m := usdLedgerMeter(st, src, c, &now)
	got := tally{}
	m.OnCharge = got.add
	c.failing = true
	if err := m.Tick(t.Context()); err == nil || c.held == nil {
		t.Fatalf("tick N: %v", err)
	}
	c.failing, c.ticking, c.at = false, 0, 2
	now, src["bbbbbbbb"] = now.Add(time.Minute), glmUsage(600_000)
	if err := m.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	spent, settled := totals(t, c)
	wantA, wantB := int64(1_400_000), int64(840_000)
	if settled["aaaaaaaa"] != wantA || settled["bbbbbbbb"] != wantB || spent != wantA+wantB || got.total() != wantA+wantB {
		t.Fatalf("spent %d, settled %v, counted %d, want %d and %d once", spent, settled, got.total(), wantA, wantB)
	}
}

// XC5: a run the store no longer lists counts as ended at the first tick that misses it, and the
// gauge keeps it an hour from then.
func TestTheSettledGaugeKeepsAnUnlistedRunAnHourFromTheFirstMiss(t *testing.T) {
	now := day22
	c := newLateCommit(ledgerDay(t, "20261022", map[string]int64{"github:bob": 5}, nil))
	st := &store{patches: map[string]map[string]string{}, runs: []runs.Run{glmRun("aaaaaaaa", now)}}
	m := usdLedgerMeter(st, typed{"aaaaaaaa": glmUsage(1_000_000)}, c, &now)
	g := &settledGauge{}
	m.OnSettled = g.set
	if err := m.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	st.runs = nil
	miss := now.Add(time.Minute)
	for _, tc := range []struct {
		at   time.Time
		kept bool
	}{{miss, true}, {miss.Add(59 * time.Minute), true}, {miss.Add(time.Hour), false}} {
		now = tc.at
		if err := m.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, ok := g.last["aaaaaaaa"]; ok != tc.kept {
			t.Fatalf("at %v: kept %v, want %v", tc.at.Sub(miss), ok, tc.kept)
		}
	}
}

// N1: the day to fence depends on when the new leader learned, not on when its first settle
// succeeds. B learns just after midnight, A's lost first write on the 22nd commits after that, and
// B's ledger stays busy past the window: the run is still charged once. The same when B learns
// before midnight and settles on the 23rd.
func TestANewLeaderFencesTheDaysItLearnedOnWhenItsFirstSettleIsLate(t *testing.T) {
	for name, tc := range map[string]struct {
		learn  time.Time // B's first tick: learns, and its settle fails
		settle time.Time // B's first successful settle
		busy   string    // the ledger whose Update stays busy on B's first tick
	}{
		"learned inside the window, settled after it":  {time.Date(2026, 10, 23, 0, 0, 30, 0, time.UTC), time.Date(2026, 10, 23, 0, 11, 30, 0, time.UTC), "20261022"},
		"learned before midnight, settled on the 23rd": {time.Date(2026, 10, 22, 23, 59, 50, 0, time.UTC), time.Date(2026, 10, 23, 0, 11, 0, 0, time.UTC), "20261022"},
	} {
		t.Run(name, func(t *testing.T) {
			now := midnight
			c := newLateCommit(ledgerDay(t, "20261022", map[string]int64{"github:bob": 5}, nil))
			src := typed{"aaaaaaaa": glmUsage(1_000_000)}
			run := glmRun("aaaaaaaa", now)
			a := usdLedgerMeter(&store{patches: map[string]map[string]string{}, runs: []runs.Run{run}}, src, c, &now)
			c.failing = true
			if err := a.Tick(t.Context()); err == nil || c.held == nil {
				t.Fatalf("leader A: %v", err)
			}
			c.failing = false
			now = tc.learn
			b := usdLedgerMeter(&store{patches: map[string]map[string]string{}, runs: []runs.Run{run}}, src, c, &now)
			c.conflicts = map[string]int{api.LedgerPrefix + tc.busy: api.LedgerAttempts}
			if err := b.Tick(t.Context()); err == nil {
				t.Fatal("B's first tick: want the busy ledger to fail it")
			}
			c.commitHeld(t.Context(), c.WithWatch) // A's write lands after B's learn
			if c.commit != nil {
				t.Fatalf("A's write: %v", c.commit)
			}
			now = tc.settle
			if err := b.Tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			if spent, settled := totals(t, c); spent != 1_400_000 || settled["aaaaaaaa"] != 1_400_000 {
				t.Fatalf("spent %d, settled %d, want the run's 1400000 once", spent, settled["aaaaaaaa"])
			}
		})
	}
}

// N3: a learned baseline that already holds a charge. A earlier settled 0.7 M of the run; A's
// next write, the other 0.7 M, is lost for good. B learns the 0.7 M, charges the rest once and is
// not netted: the guard against an under-charge.
func TestANewLeaderWithAChargeInItsBaselineStillChargesTheRest(t *testing.T) {
	now := day22
	cm := ledgerDay(t, "20261022", map[string]int64{runs.PrincipalFactory: 700_000, "github:bob": 5}, nil)
	if err := api.AddSettled(cm, map[string]int64{"aaaaaaaa": 700_000}); err != nil {
		t.Fatal(err)
	}
	c := newLateCommit(cm)
	src := typed{"aaaaaaaa": glmUsage(1_000_000)}
	run := glmRun("aaaaaaaa", now)
	a := usdLedgerMeter(&store{patches: map[string]map[string]string{}, runs: []runs.Run{run}}, src, c, &now)
	c.failing = true
	if err := a.Tick(t.Context()); err == nil || c.held == nil {
		t.Fatalf("leader A: %v", err)
	}
	c.failing, c.held = false, nil // the write is lost for good
	now = now.Add(time.Minute)
	b := usdLedgerMeter(&store{patches: map[string]map[string]string{}, runs: []runs.Run{run}}, src, c, &now)
	if err := b.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if spent, settled := totals(t, c); spent != 1_400_000 || settled["aaaaaaaa"] != 1_400_000 {
		t.Fatalf("spent %d, settled %d, want 1400000: the 0.7 M learned and the rest charged once", spent, settled["aaaaaaaa"])
	}
}
