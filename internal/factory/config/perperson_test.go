// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
)

// v2 per-person caps: off by default, the person's day defaulting to humanDailyUSD (D7), so a
// config that never names perPerson admits exactly as before.
func TestPerPersonDefaults(t *testing.T) {
	line := "budgets: {enforceTask: false, enforcePrincipal: false, factoryDailyUSD: 42.50, humanDailyUSD: 8.50}\n"
	for name, c := range map[string]struct {
		with    string
		enabled bool
		daily   float64
	}{
		"omitted":             {line, false, 8.50},
		"enabled, no value":   {strings.Replace(line, "}\n", ", perPerson: {enabled: true}}\n", 1), true, 8.50},
		"enabled, own value":  {strings.Replace(line, "}\n", ", perPerson: {enabled: true, dailyUSD: 3.40}}\n", 1), true, 3.40},
		"disabled, own value": {strings.Replace(line, "}\n", ", perPerson: {dailyUSD: 3.40}}\n", 1), false, 3.40},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := Parse([]byte(strings.Replace(good, line, c.with, 1)))
			if err != nil {
				t.Fatal(err)
			}
			if p := cfg.Budgets.PerPerson; p.Enabled != c.enabled || p.DailyUSD != c.daily {
				t.Fatalf("%+v", p)
			}
		})
	}
	cfg, err := Parse([]byte(strings.Replace(good, line, strings.Replace(line, "}\n", ", perPerson: {dailyUSD: -1}}\n", 1), 1)))
	if err == nil || !strings.Contains(err.Error(), "budgets.perPerson.dailyUSD must be a positive amount") {
		t.Fatalf("a negative person day fails the rollout: %v %+v", err, cfg)
	}
}

// DailyMicroUSD is every reader's one answer to a ledger key's day, in the ledger's micro-USD: the
// fleet's, a person's while per-person caps are on, else the human day as before.
func TestDailyMicroUSD(t *testing.T) {
	b := Budgets{FactoryDailyUSD: 42.50, HumanDailyUSD: 8.50, PerPerson: PerPerson{DailyUSD: 3.40}}
	if b.DailyMicroUSD("system:factory") != 42_500_000 || b.DailyMicroUSD("github:alice") != 8_500_000 ||
		b.DailyMicroUSD("human:291") != 8_500_000 {
		t.Fatal("off: the human day")
	}
	b.PerPerson.Enabled = true
	if b.DailyMicroUSD("system:factory") != 42_500_000 || b.DailyMicroUSD("github:alice") != 3_400_000 ||
		b.DailyMicroUSD("human:291") != 3_400_000 {
		t.Fatal("on: the person day for every non-fleet key")
	}
}

// TC7: what the ledger charges a run it cannot price, 1.7 micro-USD a token, never $0.
func TestRefMicroUSD(t *testing.T) {
	for tokens, want := range map[int64]int64{0: 0, 1000: 1700, 1_500_000: 2_550_000, 2_000_000: 3_400_000, 5_000_000: 8_500_000} {
		if got := RefMicroUSD(tokens); got != want {
			t.Errorf("RefMicroUSD(%d) = %d, want %d", tokens, got, want)
		}
	}
}
