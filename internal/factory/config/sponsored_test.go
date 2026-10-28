// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
)

// slackApp is a valid principal on good's repository, whose maintainer is Smana.
const slackApp = `  sponsored:
    - name: slack-app
      clientIDFile: /etc/agent-factory-sponsored/slack-app-client-id
      sponsor: Smana
      repositories: [Smana/cloud-native-ref]
      dailyUSD: 10
      maxCostUSD: 5
      onBehalfOf: {slack: {"T01ABC/U02DEF": smana}}
`

// withSponsored is priced with block under api:.
func withSponsored(block string) string {
	return strings.Replace(priced, "api:\n", "api:\n  projectIDFile: /etc/agent-factory-oidc/project-id\n"+block, 1)
}

// F4, R1, R3: a principal is declared in the config, bound to a maintainer, bounded in dollars.
func TestSponsoredConfig(t *testing.T) {
	cfg, err := Parse([]byte(withSponsored(slackApp)))
	if err != nil {
		t.Fatal(err)
	}
	s := cfg.SponsoredBy("system:slack-app")
	if s == nil || s.Principal() != "system:slack-app" || s.Sponsor != "Smana" || s.MaxCostUSD != 5 || s.OpenTaskCap() != DefaultSponsoredOpenTasks {
		t.Fatalf("%+v", s)
	}
	if got := cfg.Budgets.DailyMicroUSD("system:slack-app"); got != 10_000_000 {
		t.Fatalf("the principal's day is its dailyUSD: %d", got)
	}
	if got := cfg.MaxTaskMicroUSD(); got != 13_600_000 {
		t.Fatalf("frontier's taskUSD at the reference rate: %d", got)
	}
	if cfg.SponsoredBy("system:factory") != nil || cfg.SponsoredBy("github:smana") != nil {
		t.Fatal("only a declared principal is sponsored")
	}
	for name, c := range map[string]struct{ from, to, want string }{
		"a reserved name":                 {"name: slack-app", "name: runlore", `"runlore" is reserved`},
		"the broker's name":               {"name: slack-app", "name: room-broker", `"room-broker" is reserved`},
		"not a DNS label":                 {"name: slack-app", "name: Slack_App", "is not a DNS label"},
		"no client id file":               {"      clientIDFile: /etc/agent-factory-sponsored/slack-app-client-id\n", "", "clientIDFile is required"},
		"a sponsor not a maintainer":      {"sponsor: Smana", "sponsor: mallory", `sponsor "mallory" is not a maintainer of Smana/cloud-native-ref`},
		"a sponsor not a login":           {"sponsor: Smana", "sponsor: Smana_corp", `sponsor "Smana_corp" is not a GitHub login`},
		"an unlisted repository":          {"repositories: [Smana/cloud-native-ref]", "repositories: [Smana/other]", `"Smana/other" is not a listed repository`},
		"a repository spelled otherwise":  {"repositories: [Smana/cloud-native-ref]", "repositories: [smana/cloud-native-ref]", `"smana/cloud-native-ref" is not a listed repository`},
		"no day":                          {"dailyUSD: 10", "dailyUSD: 0", "dailyUSD must be above 0 and at most factoryDailyUSD"},
		"a day above the fleet's":         {"dailyUSD: 10", "dailyUSD: 1000", "dailyUSD must be above 0 and at most factoryDailyUSD"},
		"a ceiling above the day":         {"maxCostUSD: 5", "maxCostUSD: 11", "maxCostUSD must be above 0 and at most dailyUSD"},
		"a negative task cap":             {"maxCostUSD: 5\n", "maxCostUSD: 5\n      maxOpenTasks: -1\n", "maxOpenTasks must be 1 or more"},
		"a link to a non-maintainer":      {`"T01ABC/U02DEF": smana`, `"T01ABC/U02DEF": mallory`, `links "T01ABC/U02DEF" to "mallory", not a maintainer`},
		"an unknown key":                  {"dailyUSD: 10", "dailyUSD: 10\n      dayUSD: 3", `unknown field "dayUSD"`},
		"no repositories":                 {"repositories: [Smana/cloud-native-ref]", "repositories: []", "repositories is empty"},
		"a link login not a GitHub login": {`"T01ABC/U02DEF": smana`, `"T01ABC/U02DEF": smana_corp`, `"smana_corp", not a GitHub login`},
		"a NaN day":                       {"dailyUSD: 10", "dailyUSD: .nan", "unsupported value: NaN"},
		"an infinite ceiling":             {"maxCostUSD: 5", "maxCostUSD: .inf", "unsupported value: +Inf"},
		"a repository twice":              {"repositories: [Smana/cloud-native-ref]", "repositories: [Smana/cloud-native-ref, Smana/cloud-native-ref]", `"Smana/cloud-native-ref" is listed twice`},
		"a malformed Slack id":            {`"T01ABC/U02DEF"`, `"alice"`, `"alice" is not <team>/<user>`},
		"self-serve links without access": {`smana}}`, `smana}, slackLinks: true}`, "onBehalfOf.slackLinks needs api.access"},
	} {
		t.Run(name, func(t *testing.T) {
			raw := withSponsored(strings.Replace(slackApp, c.from, c.to, 1))
			if _, err := Parse([]byte(raw)); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want %q", err, c.want)
			}
		})
	}
	links := strings.Replace(withSponsored(strings.Replace(slackApp, `smana}}`, `smana}, slackLinks: true}`, 1)), "api:\n", "api:\n  access: {readerFile: /r, ttl: 5m}\n", 1)
	if cfg, err := Parse([]byte(links)); err != nil || !cfg.SponsoredBy("system:slack-app").OnBehalfOf.SlackLinks {
		t.Fatalf("self-serve links with access: %v", err)
	}
	twice := withSponsored(slackApp + strings.Replace(slackApp, "  sponsored:\n", "", 1))
	if _, err := Parse([]byte(twice)); err == nil || !strings.Contains(err.Error(), "declared twice") {
		t.Fatalf("a name twice: %v", err)
	}
	other := strings.NewReplacer("name: slack-app", "name: other-app").Replace(strings.Replace(slackApp, "  sponsored:\n", "", 1))
	if _, err := Parse([]byte(withSponsored(slackApp + other))); err == nil || !strings.Contains(err.Error(), "clientIDFile") || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("a client id file shared: %v", err)
	}
	// A sponsor is matched like a maintainer, case-insensitively.
	if _, err := Parse([]byte(withSponsored(strings.Replace(slackApp, "sponsor: Smana", "sponsor: sMANA", 1)))); err != nil {
		t.Fatalf("sponsor case: %v", err)
	}
	system := strings.Replace(withSponsored(slackApp), "api:\n",
		"api:\n  systemIssuer: https://k8s\n  systemJWKS: https://k8s/jwks\n  systemPrincipals: {sa: system:slack-app}\n", 1)
	if _, err := Parse([]byte(system)); err == nil || !strings.Contains(err.Error(), `"slack-app" is reserved`) {
		t.Fatalf("a system principal's name: %v", err)
	}
	unpriced := strings.Replace(good, "api:\n", "api:\n"+slackApp, 1)
	if _, err := Parse([]byte(unpriced)); err == nil || !strings.Contains(err.Error(), "api.sponsored needs meter.prices") {
		t.Fatalf("a ceiling in dollars without prices: %v", err)
	}
}

// R30: no principal, no sponsored day, and a hand-built config reads as before.
func TestNoSponsoredByDefault(t *testing.T) {
	cfg, err := Parse([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.API.Sponsored) != 0 || cfg.SponsoredBy("system:slack-app") != nil {
		t.Fatal("nothing is declared by default")
	}
	if got, want := cfg.Budgets.DailyMicroUSD("system:slack-app"), MicroUSD(cfg.Budgets.HumanDailyUSD); got != want {
		t.Fatalf("an undeclared key keeps today's day: %d, want %d", got, want)
	}
}

// A sponsored token's audience is pinned to the project, so the project id file is required with it.
func TestSponsoredNeedsProjectIDFile(t *testing.T) {
	raw := strings.Replace(priced, "api:\n", "api:\n"+slackApp, 1)
	if _, err := Parse([]byte(raw)); err == nil || !strings.Contains(err.Error(), "api.projectIDFile") {
		t.Fatalf("got %v", err)
	}
}
