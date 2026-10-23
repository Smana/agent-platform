// SPDX-License-Identifier: Apache-2.0

package meter

import (
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/runs"
)

// sponsoredConfig is a parsed config declaring system:slack-app with a $10 day, per-person caps,
// enforceTask and enforcePrincipal all off.
func sponsoredConfig(t *testing.T) *config.Config {
	t.Helper()
	c, err := config.Load("../config/testdata/sponsored.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// F4, C-1: a sponsored run is revoked at its own USD cap (a third of its ceiling), though its tier's
// cap is $2.55; the same usage on a run without one is left running.
func TestASponsoredRunIsRevokedAtItsOwnCap(t *testing.T) {
	u := typedUsage(Usage{Input: 400_000, Output: 30_000}) // $0.56 + $0.13: past $0.666, far under $2.55
	for name, c := range map[string]struct {
		cap  int64
		want []revocation
	}{
		"the ceiling's third": {666_666, []revocation{{ReasonBudgetRun, LimitRunUSD}}},
		"no run cap":          {0, nil},
	} {
		t.Run(name, func(t *testing.T) {
			r := standardRun("aaaaaaaa")
			r.MaxMicroUSD = c.cap
			st := &store{patches: map[string]map[string]string{}, runs: []runs.Run{r}}
			var revoked []revocation
			var fell []string
			if err := usdMeter(st, typed{"aaaaaaaa": u}, &revoked, &fell).Tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			if len(revoked) != len(c.want) || (len(c.want) > 0 && revoked[0] != c.want[0]) {
				t.Fatalf("revoked %v, want %v", revoked, c.want)
			}
		})
	}
}

// F4, C-1: a run cap holds a tier that has no USD cap too, when the run is priced.
func TestARunCapHoldsATierWithoutAUSDCap(t *testing.T) {
	r := standardRun("aaaaaaaa")
	r.MaxMicroUSD = 666_666
	st := &store{patches: map[string]map[string]string{}, runs: []runs.Run{r}}
	var revoked []revocation
	var fell []string
	m := usdMeter(st, typed{"aaaaaaaa": typedUsage(Usage{Input: 400_000, Output: 30_000})}, &revoked, &fell)
	m.RunUSD = nil
	if err := m.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(revoked) != 1 || revoked[0] != (revocation{ReasonBudgetRun, LimitRunUSD}) {
		t.Fatalf("revoked %v", revoked)
	}
}

// F4, R7, C-1: a sponsored principal's day stops its runs whatever EnforcePrincipal says; a run
// of an unsponsored key on a spent day is not stopped while that flag is off (R3's shadow).
func TestASponsoredPrincipalsDayRevokesWithoutEnforcePrincipal(t *testing.T) {
	day := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	cfg := sponsoredConfig(t)
	if cfg.Budgets.EnforcePrincipal {
		t.Fatal("the fixture's flag is off")
	}
	st := &store{patches: map[string]map[string]string{}, runs: []runs.Run{
		{ID: "aaaaaaaa", Phase: "Running", MaxTokens: 5_000_000, Principal: "system:factory", ChargedTo: "system:slack-app", MaxMicroUSD: 666_666, Created: day},
		{ID: "bbbbbbbb", Phase: "Running", MaxTokens: 5_000_000, Principal: "system:factory", ChargedTo: "github:alice", Created: day},
	}}
	c := fake.NewClientBuilder().WithScheme(cmScheme()).WithObjects(
		ledgerDay(t, "20260927", map[string]int64{"system:slack-app": 10_000_000, "github:alice": 99_000_000}, nil)).Build()
	m := &Meter{Runs: st, Source: source{"aaaaaaaa": 1_000, "bbbbbbbb": 1_000}, Budgets: cfg.Budgets, Ledger: c, Namespace: "agent-system",
		Now: func() time.Time { return day }}
	if err := m.Tick(t.Context()); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{"aaaaaaaa": "budget-principal", "bbbbbbbb": ""} {
		if got := st.patches[id][runs.AnnRevoked]; got != want {
			t.Errorf("%s: %q, want %q", id, got, want)
		}
	}
}

// N-1: a run charged to a sponsored principal whose own cap is missing, unreadable or not positive
// is revoked, never left on its tier's cap; a run of an unsponsored key without one is untouched.
func TestASponsoredRunWithoutItsCapIsRevoked(t *testing.T) {
	cfg := sponsoredConfig(t)
	for name, c := range map[string]struct {
		chargedTo string
		cap       int64
		want      string
	}{
		"missing":                 {"system:slack-app", 0, "budget-run"},
		"garbage or non-positive": {"system:slack-app", -1, "budget-run"},
		"present":                 {"system:slack-app", 666_666, ""},
		"an unsponsored run":      {"github:alice", 0, ""},
	} {
		t.Run(name, func(t *testing.T) {
			r := standardRun("aaaaaaaa")
			r.Principal, r.ChargedTo, r.MaxMicroUSD = runs.PrincipalFactory, c.chargedTo, c.cap
			st := &store{patches: map[string]map[string]string{}, runs: []runs.Run{r}}
			var revoked []revocation
			var fell []string
			m := usdMeter(st, typed{"aaaaaaaa": typedUsage(Usage{Input: 1_000, Output: 100})}, &revoked, &fell)
			m.Budgets = cfg.Budgets
			if err := m.Tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			if got := st.patches["aaaaaaaa"][runs.AnnRevoked]; got != c.want {
				t.Fatalf("revoked %q, want %q", got, c.want)
			}
		})
	}
}
