// SPDX-License-Identifier: Apache-2.0

package meter

import (
	"context"
	"errors"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/Smana/agent-platform/internal/factory/api"
	"github.com/Smana/agent-platform/internal/factory/runs"
)

// lateCommit is a client over objs whose first Update of a day ledger while failing answers 504
// without committing, and keeps the object so the test commits it later: the apiserver's "unable
// to return a response in the time allotted", whose write lands after all (#166). ticking is the
// number of ledger reads the current tick made; at, when non-zero, commits the held write from
// that read of the tick, before it is answered.
type lateCommit struct {
	client.WithWatch
	held    client.Object
	failing bool
	ticking int
	at      int
	commit  error
}

func newLateCommit(objs ...client.Object) *lateCommit {
	l := &lateCommit{}
	l.WithWatch = fake.NewClientBuilder().WithScheme(cmScheme()).WithObjects(objs...).WithInterceptorFuncs(interceptor.Funcs{
		Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			if l.failing && l.held == nil {
				l.held = obj.DeepCopyObject().(client.Object)
				return errors.New("the server was unable to return a response in the time allotted")
			}
			return cl.Update(ctx, obj, opts...)
		},
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if _, ok := api.LedgerDay(key.Name, api.LedgerPrefix); ok {
				l.ticking++
				if l.at != 0 && l.ticking == l.at && l.held != nil {
					l.commitHeld(ctx, cl)
				}
			}
			return cl.Get(ctx, key, obj, opts...)
		},
	}).Build()
	return l
}

func (l *lateCommit) commitHeld(ctx context.Context, cl client.WithWatch) {
	l.commit = cl.Update(ctx, l.held)
	l.held = nil
}

// #166, the reviewer's sequence: tick N's Update answers 504 and commits later, from the read that
// follows tick N+1's read-back of the ledger. The run's 1.4 M is charged once, however the late
// write falls: before the next write reads the ledger, between its read and its Update, or after
// it. Across midnight the failed write's day is the one the late write lands on.
func TestReviewParked1LateCommit(t *testing.T) {
	for name, tc := range map[string]struct {
		start time.Time
		grow  int64 // what the run read in tokens between the ticks
		at    int   // the read of tick N+1 that finds the late write committed; 0: after the tick
	}{
		"same day, the commit follows the read-back":               {time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC), 0, 2},
		"same day, the commit comes after the write":               {time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC), 0, 0},
		"same day, the run grew, the commit follows the read-back": {time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC), 100000, 2},
		"across midnight, the commit follows the read-back":        {time.Date(2026, 10, 22, 23, 59, 30, 0, time.UTC), 0, 2},
		"across midnight, the run grew, the commit follows":        {time.Date(2026, 10, 22, 23, 59, 30, 0, time.UTC), 100000, 2},
		"across midnight, the commit comes after the tick":         {time.Date(2026, 10, 22, 23, 59, 30, 0, time.UTC), 0, 0},
	} {
		t.Run(name, func(t *testing.T) {
			now := tc.start
			day := now.Format(api.LedgerDayLayout)
			// The day's ledger exists, so the write is an Update that carries a resourceVersion.
			c := newLateCommit(ledgerDay(t, day, map[string]int64{"github:bob": 5}, nil))
			m := usdLedgerMeter(&store{patches: map[string]map[string]string{}, runs: []runs.Run{glmRun("aaaaaaaa", now)}},
				typed{"aaaaaaaa": glmUsage(1_000_000)}, c, &now)
			got := tally{}
			m.OnCharge = got.add
			c.failing = true
			if err := m.Tick(t.Context()); err == nil || c.held == nil {
				t.Fatalf("tick N: the 504 is reported and the write held: %v", err)
			}
			c.failing, c.ticking, c.at = false, 0, tc.at
			now = now.Add(time.Minute)
			m.Source.(typed)["aaaaaaaa"] = glmUsage(1_000_000 + tc.grow)
			want := 1_400_000 + tc.grow*14/10 // 1.4 micro-USD a token
			if err := m.Tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			if c.held != nil { // the commit after the tick
				c.commitHeld(t.Context(), c.WithWatch)
				if !apierrors.IsConflict(c.commit) {
					t.Fatalf("the late write, after the tick's write: %v, want a conflict", c.commit)
				}
			}
			if tc.at != 0 && c.commit != nil {
				t.Fatalf("the late write: %v", c.commit)
			}
			var total, settled int64
			for _, d := range []string{"20261022", "20261023"} {
				if s, ok := spentOf(t, c, d); ok {
					total += s[runs.PrincipalFactory]
					settled += settledOf(t, c, d)["aaaaaaaa"]
				}
			}
			if total != want || settled != want {
				t.Fatalf("spent %d, settled %d, want the run's %d once", total, settled, want)
			}
			if got.total() != want {
				t.Fatalf("counted %d, want %d", got.total(), want)
			}
		})
	}
}

// settledGauge is the last OnSettled hand-over, by run.
type settledGauge struct{ last map[string]float64 }

func (g *settledGauge) set(s map[string]float64) { g.last = s }

// XC5: the settled gauge is the ledger's total for the run, which a double charge doubles: the
// reviewer's sequence reads 1.4 once the fix holds. (With the fix reverted the same test reads
// 2.8, the ledger's count and not the meter's own.)
func TestTheSettledGaugeReadsTheLedgerAfterALateCommit(t *testing.T) {
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	c := newLateCommit(ledgerDay(t, "20261022", map[string]int64{"github:bob": 5}, nil))
	m := usdLedgerMeter(&store{patches: map[string]map[string]string{}, runs: []runs.Run{glmRun("aaaaaaaa", now)}},
		typed{"aaaaaaaa": glmUsage(1_000_000)}, c, &now)
	g := &settledGauge{}
	m.OnSettled = g.set
	c.failing = true
	if err := m.Tick(t.Context()); err == nil || c.held == nil {
		t.Fatalf("tick N: the 504 is reported and the write held: %v", err)
	}
	if got, ok := g.last["aaaaaaaa"]; ok {
		t.Fatalf("a write that reported an error showed %v", got)
	}
	c.failing, c.ticking, c.at = false, 0, 2
	now = now.Add(time.Minute)
	if err := m.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := g.last["aaaaaaaa"]; got != 1.4 {
		t.Fatalf("the gauge reads %v, want the run's 1.4 once", got)
	}
}

// XC5: the gauge sums the run's entries across days, a new leader reads it back, and the run
// leaves an hour after it ends.
func TestTheSettledGaugeSumsDaysSurvivesAFailoverAndLeavesAfterAnHour(t *testing.T) {
	now := time.Date(2026, 10, 22, 23, 59, 30, 0, time.UTC)
	c := fake.NewClientBuilder().WithScheme(cmScheme()).Build()
	run := glmRun("aaaaaaaa", now)
	st := &store{patches: map[string]map[string]string{}, runs: []runs.Run{run}}
	src := typed{"aaaaaaaa": glmUsage(1_000_000)}
	m := usdLedgerMeter(st, src, c, &now)
	g := &settledGauge{}
	m.OnSettled = g.set
	if err := m.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	now, src["aaaaaaaa"] = now.Add(time.Minute), glmUsage(1_100_000)
	if err := m.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := g.last["aaaaaaaa"]; got != 1.54 { // 1.4 on the 22nd, 0.14 on the 23rd
		t.Fatalf("the gauge reads %v, want 1.54", got)
	}
	run.Tokens = 1_100_000
	next := usdLedgerMeter(&store{patches: map[string]map[string]string{}, runs: []runs.Run{run}}, src, c, &now)
	ng := &settledGauge{}
	next.OnSettled = ng.set
	if err := next.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if got := ng.last["aaaaaaaa"]; got != 1.54 {
		t.Fatalf("a new leader's gauge reads %v, want 1.54", got)
	}
	run.Phase, run.Finished = "Succeeded", now
	next.Runs = &store{patches: map[string]map[string]string{}, runs: []runs.Run{run}}
	for _, tc := range []struct {
		after time.Duration
		kept  bool
	}{{59 * time.Minute, true}, {time.Hour, false}} {
		now = run.Finished.Add(tc.after)
		if err := next.Tick(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, ok := ng.last["aaaaaaaa"]; ok != tc.kept {
			t.Fatalf("%v after the end: kept %v, want %v", tc.after, ok, tc.kept)
		}
	}
}
