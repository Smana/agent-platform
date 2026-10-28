// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
)

// runloreBlock is the design's entry, under api:.
const runloreBlock = `  internalPrincipals:
    - name: runlore
      clientIDFile: /etc/agent-factory-sponsored/runlore-client-id
      sponsor: Smana
      repositories: [Smana/cloud-native-ref]
      dailyUSD: 10
      maxCostUSD: 2
      maxOpenTasks: 3
      minConfidence: 0.75
      dailyTasks: 5
      verification: true
`

// RA2, RA3: an internal principal is declared beside the sponsored ones, bounded the same way.
func TestInternalPrincipalConfig(t *testing.T) {
	cfg, err := Parse([]byte(withSponsored(runloreBlock)))
	if err != nil {
		t.Fatal(err)
	}
	ip := cfg.InternalBy("system:runlore")
	if ip == nil || ip.MinConfidence != 0.75 || ip.DailyTasks != 5 || !ip.Verification || ip.OpenTaskCap() != 3 {
		t.Fatalf("%+v", ip)
	}
	if p := cfg.PrincipalBy("system:runlore"); p == nil || p.Sponsor != "Smana" || p.MaxCostUSD != 2 {
		t.Fatalf("PrincipalBy: %+v", p)
	}
	if cfg.SponsoredBy("system:runlore") != nil {
		t.Fatal("an internal principal is not a sponsored one")
	}
	if !cfg.Budgets.Sponsored("system:runlore") || cfg.Budgets.DailyMicroUSD("system:runlore") != 10_000_000 {
		t.Fatal("its day is its dailyUSD, a bound as a sponsored principal's")
	}
	for name, c := range map[string]struct{ from, to, want string }{
		"confidence 0":         {"minConfidence: 0.75", "minConfidence: 0", "minConfidence must be in (0, 1]"},
		"confidence above 1":   {"minConfidence: 0.75", "minConfidence: 1.5", "minConfidence must be in (0, 1]"},
		"no daily tasks":       {"dailyTasks: 5", "dailyTasks: 0", "dailyTasks must be 1 or more"},
		"an onBehalfOf":        {"verification: true\n", "verification: true\n      onBehalfOf: {slack: {\"T01ABC/U02DEF\": smana}}\n", "onBehalfOf is not allowed"},
		"self-serve links":     {"verification: true\n", "verification: true\n      onBehalfOf: {slackLinks: true}\n", "onBehalfOf is not allowed"},
		"a reserved name":      {"name: runlore", "name: factory", `"factory" is reserved`},
		"a ceiling over a day": {"maxCostUSD: 2", "maxCostUSD: 11", "maxCostUSD must be above 0 and at most dailyUSD"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(withSponsored(strings.Replace(runloreBlock, c.from, c.to, 1))))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want %q, got %v", c.want, err)
			}
		})
	}
}

// R2: a name and a client id file are one principal's, across both lists.
func TestInternalAndSponsoredNamesAreUnique(t *testing.T) {
	twice := strings.Replace(runloreBlock, "name: runlore", "name: slack-app", 1)
	if _, err := Parse([]byte(withSponsored(slackApp + twice))); err == nil || !strings.Contains(err.Error(), `"slack-app" is declared twice`) {
		t.Fatalf("a name in both lists: %v", err)
	}
	shared := strings.Replace(runloreBlock, "runlore-client-id", "slack-app-client-id", 1)
	if _, err := Parse([]byte(withSponsored(slackApp + shared))); err == nil || !strings.Contains(err.Error(), "is already used by slack-app") {
		t.Fatalf("a shared client id file: %v", err)
	}
	if _, err := Parse([]byte(withSponsored(slackApp + runloreBlock))); err != nil {
		t.Fatalf("both lists side by side: %v", err)
	}
}
