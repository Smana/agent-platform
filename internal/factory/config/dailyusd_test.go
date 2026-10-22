// SPDX-License-Identifier: Apache-2.0

package config

import (
	"slices"
	"strings"
	"testing"
)

// TC5: the days are in USD, defaulting to OD-10's 25 M and 5 M tokens at the reference $1.70 per
// million, and a person's day to the human one.
func TestTheDailyCapsAreInUSD(t *testing.T) {
	line := "budgets: {enforceTask: false, enforcePrincipal: false, factoryDailyUSD: 42.50, humanDailyUSD: 8.50}\n"
	for name, c := range map[string]struct {
		with                   string
		factory, human, person float64
	}{
		"omitted":      {"", 42.50, 8.50, 8.50},
		"written":      {"budgets: {factoryDailyUSD: 60, humanDailyUSD: 10, perPerson: {dailyUSD: 4.25}}\n", 60, 10, 4.25},
		"person, none": {"budgets: {humanDailyUSD: 10}\n", 42.50, 10, 10},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := Parse([]byte(strings.Replace(good, line, c.with, 1)))
			if err != nil {
				t.Fatal(err)
			}
			b := cfg.Budgets
			if b.FactoryDailyUSD != c.factory || b.HumanDailyUSD != c.human || b.PerPerson.DailyUSD != c.person {
				t.Fatalf("%+v, want $%v, $%v, person $%v", b, c.factory, c.human, c.person)
			}
			if len(cfg.Ignored) != 0 {
				t.Fatalf("ignored %v on a file that writes no token day", cfg.Ignored)
			}
		})
	}
}

// TC5: one release accepts the token days, ignores them and names each, so a file written for the
// previous release still rolls out; the strict decoder refuses them once the fields are gone.
func TestTheTokenDaysAreAcceptedIgnoredAndNamed(t *testing.T) {
	line := "budgets: {enforceTask: false, enforcePrincipal: false, factoryDailyUSD: 42.50, humanDailyUSD: 8.50}\n"
	old := "budgets: {enforcePrincipal: false, factoryDaily: 25000000, humanDaily: 5000000, perPerson: {enabled: true, dailyTokens: 2000000}}\n"
	cfg, err := Parse([]byte(strings.Replace(good, line, old, 1)))
	if err != nil {
		t.Fatal(err)
	}
	want := []Ignored{
		{Key: "budgets.factoryDaily", Use: "budgets.factoryDailyUSD"},
		{Key: "budgets.humanDaily", Use: "budgets.humanDailyUSD"},
		{Key: "budgets.perPerson.dailyTokens", Use: "budgets.perPerson.dailyUSD"},
	}
	if !slices.Equal(cfg.Ignored, want) {
		t.Fatalf("ignored %v, want %v", cfg.Ignored, want)
	}
	b := cfg.Budgets
	if b.FactoryDailyUSD != 42.50 || b.HumanDailyUSD != 8.50 || b.PerPerson.DailyUSD != 8.50 {
		t.Fatalf("a token day is ignored, never converted: %+v", b)
	}
	if b.IgnoredFactoryDaily != nil || b.IgnoredHumanDaily != nil || b.PerPerson.IgnoredDailyTokens != nil {
		t.Fatalf("an ignored key is cleared, so nothing reads it: %+v", b)
	}
	one := strings.Replace(good, line, "budgets: {factoryDailyUSD: 42.50, humanDaily: 5000000}\n", 1)
	if cfg, err := Parse([]byte(one)); err != nil || !slices.Equal(cfg.Ignored, want[1:2]) {
		t.Fatalf("one token day beside the USD days: %v %v", err, cfg)
	}
}
