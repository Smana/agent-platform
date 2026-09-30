// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const good = `
repository: Smana/cloud-native-ref
maintainers: [Smana]
triggerLabel: factory/ready
factoryLogin: ogenki-agent-factory[bot]
agentsLogin: ogenki-agents[bot]
roomsURL: https://rooms.priv.gcp.ogenki.io
broker: {url: "https://room-broker.agent-system.svc.cluster.local:8443", caFile: /etc/agent-factory/openbao-ca/ca.crt, tokenFile: /var/run/secrets/agents/rooms/token}
github: {appIDFile: /etc/agent-factory-github/app_id, privateKeyFile: /etc/agent-factory-github/private_key}
poll: {issues: 60s, tasks: 30s, meter: 30s}
defaults: {template: solo, tier: standard, dataClass: public, predictedClass: review}
tiers:
  light:    {model: agent-default, runTokens: 300000,  taskTokens: 600000,  runMinutes: 20}
  standard: {model: agent-default, runTokens: 1500000, taskTokens: 3000000, runMinutes: 45}
  frontier: {model: agent-default, runTokens: 4000000, taskTokens: 8000000, runMinutes: 90}
templates:
  solo: {roles: [implementer]}
  pair: {roles: [implementer, reviewer], maxReviewRounds: 2}
  trio: {roles: [implementer, tester, reviewer], maxReviewRounds: 2}
  investigate: {roles: [triager]}  # R38: proposes a public issue text, never writes
caps: {activeTasks: 3, concurrentRuns: 4, tasksPerDay: 20, maxTextBytes: 14336}
meter:
  url: http://vmsingle-victoria-metrics-k8s-stack.observability.svc:8428
  query: 'sum by (ar_agent) (gen_ai_client_token_usage_sum{ar_agent=~"system:serviceaccount:agents:xplane-run-.+", gen_ai_token_type=~"input|output"})'
`

func TestGoodConfigParses(t *testing.T) {
	c, err := Parse([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if c.Poll.Issues.Seconds() != 60 || c.Tiers["standard"].RunTokens != 1_500_000 || c.Templates["pair"].MaxReviewRounds != 2 {
		t.Fatalf("%+v", c)
	}
	if c.Broker.CAFile != "/etc/agent-factory/openbao-ca/ca.crt" {
		t.Fatalf("broker.caFile = %q", c.Broker.CAFile)
	}
	if !c.IsMaintainer("smana") || c.IsMaintainer("someone") {
		t.Fatal("maintainers match case-insensitively, and only listed logins")
	}
	if len(c.Hash) != 64 {
		t.Fatal("the config hash stamps each task (breaker)")
	}
}

func TestLoadReadsTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil || c.Repository != "Smana/cloud-native-ref" {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("a missing file fails startup")
	}
}

// The rules refuse only what they name: each variant here is a valid config.
func TestGoodVariantsParse(t *testing.T) {
	for name, c := range map[string][2]string{
		"internal data":        {"dataClass: public", "dataClass: internal"},
		"implementer anywhere": {"pair: {roles: [implementer, reviewer]", "pair: {roles: [reviewer, implementer]"},
	} {
		t.Run(name, func(t *testing.T) {
			raw := strings.Replace(good, c[0], c[1], 1)
			if raw == good {
				t.Fatal("the edit did not apply")
			}
			if _, err := Parse([]byte(raw)); err != nil {
				t.Error(err)
			}
		})
	}
}

// A bad config fails its rollout (§4): unknown keys, caps above the platform's, gaps. Each
// case names the error its own rule reports, so no rule hides behind another that fires on
// the same edit (review I2: a rule deleted must fail a test).
func TestBadConfigsFail(t *testing.T) {
	for name, c := range map[string]struct{ from, to, err string }{
		"unknown key":           {"triggerLabel: factory/ready", "triggerLabel: factory/ready\ntrigerLabel: x", `unknown field "trigerLabel"`},
		"run tokens > ceiling":  {"runTokens: 4000000", "runTokens: 6000000", "tier frontier: runTokens must be 1..5000000"},
		"run tokens below one":  {"runTokens: 300000,", "runTokens: 0,", "tier light: runTokens must be 1..5000000"},
		"task below run tokens": {"taskTokens: 600000,", "taskTokens: 200000,", "tier light: taskTokens below runTokens"},
		"text above the XRD":    {"maxTextBytes: 14336", "maxTextBytes: 32768", "caps.maxTextBytes must be 1..14336"},
		"text below one byte":   {"maxTextBytes: 14336", "maxTextBytes: 0", "caps.maxTextBytes must be 1..14336"},
		"unknown role":          {"roles: [implementer]}", "roles: [implementor]}", `template solo: unknown role "implementor"`},
		"missing tier":          {"  light:    {model: agent-default, runTokens: 300000,  taskTokens: 600000,  runMinutes: 20}\n", "", "tier light is missing"},
		"a fourth tier": {"tiers:\n", "tiers:\n  huge: {model: agent-default, runTokens: 1, taskTokens: 1, runMinutes: 1}\n",
			"tiers are exactly light, standard and frontier"},
		"minutes above 480":      {"runMinutes: 90", "runMinutes: 600", "tier frontier: runMinutes must be 1..480"},
		"minutes below one":      {"runMinutes: 90", "runMinutes: 0", "tier frontier: runMinutes must be 1..480"},
		"default template gone":  {"template: solo", "template: duo", `defaults.template "duo" is not a template`},
		"no maintainer":          {"maintainers: [Smana]", "maintainers: []", "maintainers is empty"},
		"bad duration":           {"issues: 60s", "issues: 60", "a duration is a string"},
		"unknown model":          {"model: agent-default, runTokens: 1500000", "model: gpt-5, runTokens: 1500000", `tier standard: model "gpt-5" is not a C5 logical name`},
		"template without roles": {"solo: {roles: [implementer]}", "solo: {roles: []}", "template solo has no roles"},
		// R38 (owner default): only a lone triager may go without an implementer.
		"a reviewer without an implementer": {"investigate: {roles: [triager]}", "investigate: {roles: [reviewer]}",
			"template investigate: only an implementer writes"},
		"a triager with a reviewer": {"investigate: {roles: [triager]}", "investigate: {roles: [triager, reviewer]}",
			"template investigate: only an implementer writes"},
		"unknown default tier":      {"tier: standard,", "tier: medium,", `defaults.tier "medium" is not a tier`},
		"unknown data class":        {"dataClass: public", "dataClass: secret", "defaults.dataClass is public or internal"},
		"no predicted class":        {", predictedClass: review}", "}", "defaults.predictedClass is required"},
		"no active tasks":           {"activeTasks: 3", "activeTasks: 0", "caps must be positive"},
		"no concurrent runs":        {"concurrentRuns: 4", "concurrentRuns: 0", "caps must be positive"},
		"no tasks per day":          {"tasksPerDay: 20", "tasksPerDay: 0", "caps must be positive"},
		"issues poll not positive":  {"issues: 60s", "issues: 0s", "poll.issues must be positive"},
		"tasks poll negative":       {"tasks: 30s", "tasks: -1s", "poll.tasks must be positive"},
		"meter poll not positive":   {"meter: 30s", "meter: 0s", "poll.meter must be positive"},
		"repository not owner/name": {"repository: Smana/cloud-native-ref", "repository: ../x", `repository "../x" is not owner/name`},
		"repository with a path":    {"repository: Smana/cloud-native-ref", "repository: Smana/cloud-native-ref/x", "is not owner/name"},
		// Ruling SC: the broker's :8443 is TLS, verified against the mounted CA.
		"broker not https":       {`url: "https://room-broker`, `url: "http://room-broker`, "must be an https:// URL"},
		"broker without host":    {`"https://room-broker.agent-system.svc.cluster.local:8443"`, `"https://"`, `broker.url "https://" must be an https:// URL`},
		"no broker URL":          {`"https://room-broker.agent-system.svc.cluster.local:8443"`, `""`, "broker.url is required"},
		"broker without a CA":    {"caFile: /etc/agent-factory/openbao-ca/ca.crt, ", "", "broker.caFile is required"},
		"broker without a token": {", tokenFile: /var/run/secrets/agents/rooms/token", "", "broker.tokenFile is required"},
		"no trigger label":       {"triggerLabel: factory/ready", `triggerLabel: ""`, "triggerLabel is required"},
		"no factory login":       {"factoryLogin: ogenki-agent-factory[bot]", "factoryLogin: ''", "factoryLogin is required"},
		"no agents login":        {"agentsLogin: ogenki-agents[bot]", "agentsLogin: ''", "agentsLogin is required"},
		"no rooms URL":           {"roomsURL: https://rooms.priv.gcp.ogenki.io", "roomsURL: ''", "roomsURL is required"},
		"no app id file":         {"appIDFile: /etc/agent-factory-github/app_id", "appIDFile: ''", "github.appIDFile is required"},
		"no private key file":    {"privateKeyFile: /etc/agent-factory-github/private_key", "privateKeyFile: ''", "github.privateKeyFile is required"},
		"no meter URL":           {"url: http://vmsingle-victoria-metrics-k8s-stack.observability.svc:8428", "url: ''", "meter.url is required"},
		"no meter query": {`query: 'sum by (ar_agent) (gen_ai_client_token_usage_sum{ar_agent=~"system:serviceaccount:agents:xplane-run-.+", gen_ai_token_type=~"input|output"})'`,
			"query: ''", "meter.query is required"},
	} {
		t.Run(name, func(t *testing.T) {
			raw := strings.Replace(good, c.from, c.to, 1)
			if raw == good {
				t.Fatal("the edit did not apply")
			}
			_, err := Parse([]byte(raw))
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Errorf("err = %v, want one naming %q", err, c.err)
			}
		})
	}
}
