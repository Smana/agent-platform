// SPDX-License-Identifier: Apache-2.0

package meter

import (
	"context"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/runs"
)

// v2 per-person caps G4: a run charged to a person settles its tokens to the fleet and to that
// person, never to its gateway principal; the person's day past its cap revokes the run (D5),
// and the gauge reports the person's remaining day.
func TestAChargedRunSettlesToThePersonAndTheFleet(t *testing.T) {
	day := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	st := &store{patches: map[string]map[string]string{}, runs: []runs.Run{
		{ID: "aaaaaaaa", Phase: "Running", MaxTokens: 5_000_000, Principal: "system:factory", ChargedTo: "github:alice", Created: day},
		{ID: "bbbbbbbb", Phase: "Running", MaxTokens: 5_000_000, Principal: "human:291", ChargedTo: "github:alice", Created: day},
		{ID: "cccccccc", Phase: "Running", MaxTokens: 5_000_000, Principal: "system:factory", Created: day}, // a schedule's: the fleet alone
	}}
	c := fake.NewClientBuilder().WithScheme(cmScheme()).WithObjects(
		ledgerDay(t, "20260927", map[string]int64{"github:alice": 1_800_000}, nil)).Build()
	remaining := map[string]int64{}
	m := &Meter{Runs: st, Source: source{"aaaaaaaa": 100_000, "bbbbbbbb": 100_000, "cccccccc": 50_000},
		Budgets: config.Budgets{EnforcePrincipal: true, FactoryDaily: 25_000_000, HumanDaily: 5_000_000,
			PerPerson: config.PerPerson{Enabled: true, DailyTokens: 2_000_000}},
		Ledger: c, Namespace: "agent-system",
		Now: func() time.Time { return day }, Remaining: func(p string, n, _ int64) { remaining[p] = n }}
	if err := m.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	spent, _ := spentOf(t, c, "20260927")
	if spent["github:alice"] != 2_000_000 || spent["system:factory"] != 250_000 || spent["human:291"] != 0 {
		t.Fatalf("ledger %v", spent)
	}
	for id, want := range map[string]string{"aaaaaaaa": "budget-principal", "bbbbbbbb": "budget-principal", "cccccccc": ""} {
		if got := st.patches[id][runs.AnnRevoked]; got != want {
			t.Errorf("%s: %q, want %q", id, got, want)
		}
	}
	if remaining["github:alice"] != 0 || remaining["system:factory"] != 25_000_000-250_000 {
		t.Fatalf("%v", remaining)
	}
}
