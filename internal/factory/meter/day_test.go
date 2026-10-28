// SPDX-License-Identifier: Apache-2.0

package meter

import (
	"maps"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Smana/maku/internal/factory/config"
	"github.com/Smana/maku/internal/factory/runs"
)

// TC8: a gauge moves only when recorded. At a day's first tick each principal of the day before
// that has spent nothing yet today reads 0, so an 80% alert does not outlive midnight; one that
// spends today reads today's spend alone.
func TestYesterdaysPrincipalsReadZeroAtTheDaysFirstTick(t *testing.T) {
	now := time.Date(2026, 10, 22, 23, 59, 30, 0, time.UTC)
	c := fake.NewClientBuilder().WithScheme(cmScheme()).Build()
	st := &store{patches: map[string]map[string]string{}, runs: []runs.Run{
		{ID: "aaaaaaaa", Phase: "Running", Principal: runs.PrincipalFactory, Created: now},
		{ID: "bbbbbbbb", Phase: "Running", Principal: "human:1", Created: now},
	}}
	src := source{"aaaaaaaa": 1000, "bbbbbbbb": 1000}
	type told struct{ spent, limit int64 }
	day := map[string]told{}
	m := &Meter{Runs: st, Source: src, Ledger: c, Namespace: "agent-system",
		Budgets: config.Budgets{FactoryDailyUSD: 42.50, HumanDailyUSD: 8.50},
		Now:     func() time.Time { return now },
		Day:     func(p string, spent, limit int64) { day[p] = told{spent, limit} }}
	if err := m.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if want := map[string]told{runs.PrincipalFactory: {1700, 42_500_000}, "human:1": {1700, 8_500_000}}; !maps.Equal(day, want) {
		t.Fatalf("the first day: %v, want %v", day, want)
	}
	now = now.Add(time.Minute) // 2026-10-23 00:00:30: only the fleet's run spends
	src["aaaaaaaa"] = 2000
	if err := m.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if want := map[string]told{runs.PrincipalFactory: {1700, 42_500_000}, "human:1": {0, 8_500_000}}; !maps.Equal(day, want) {
		t.Fatalf("after midnight: %v, want %v", day, want)
	}
}
