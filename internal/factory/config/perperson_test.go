// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
)

// v2 per-person caps: off by default, the person's day defaulting to humanDaily (D7), so a config
// that never names perPerson admits exactly as before.
func TestPerPersonDefaults(t *testing.T) {
	line := "budgets: {enforceTask: false, enforcePrincipal: false, factoryDaily: 25000000, humanDaily: 5000000}\n"
	for name, c := range map[string]struct {
		with    string
		enabled bool
		daily   int64
	}{
		"omitted":             {line, false, 5_000_000},
		"enabled, no value":   {strings.Replace(line, "}\n", ", perPerson: {enabled: true}}\n", 1), true, 5_000_000},
		"enabled, own value":  {strings.Replace(line, "}\n", ", perPerson: {enabled: true, dailyTokens: 2000000}}\n", 1), true, 2_000_000},
		"disabled, own value": {strings.Replace(line, "}\n", ", perPerson: {dailyTokens: 2000000}}\n", 1), false, 2_000_000},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := Parse([]byte(strings.Replace(good, line, c.with, 1)))
			if err != nil {
				t.Fatal(err)
			}
			if p := cfg.Budgets.PerPerson; p.Enabled != c.enabled || p.DailyTokens != c.daily {
				t.Fatalf("%+v", p)
			}
		})
	}
	cfg, err := Parse([]byte(strings.Replace(good, line, strings.Replace(line, "}\n", ", perPerson: {dailyTokens: -1}}\n", 1), 1)))
	if err == nil || !strings.Contains(err.Error(), "budgets.perPerson.dailyTokens must be positive") {
		t.Fatalf("a negative person day fails the rollout: %v %+v", err, cfg)
	}
}

// DailyCap is every reader's one answer to a ledger key's day: the fleet's, a person's while
// per-person caps are on, else the human day as before.
func TestDailyCap(t *testing.T) {
	b := Budgets{FactoryDaily: 25, HumanDaily: 5, PerPerson: PerPerson{DailyTokens: 2}}
	if b.DailyCap("system:factory") != 25 || b.DailyCap("github:alice") != 5 || b.DailyCap("human:291") != 5 {
		t.Fatal("off: the human day")
	}
	b.PerPerson.Enabled = true
	if b.DailyCap("system:factory") != 25 || b.DailyCap("github:alice") != 2 || b.DailyCap("human:291") != 2 {
		t.Fatal("on: the person day for every non-fleet key")
	}
}
