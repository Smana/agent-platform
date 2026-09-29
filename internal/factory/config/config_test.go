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

// A bad config fails its rollout (§4): unknown keys, caps above the platform's, gaps.
func TestBadConfigsFail(t *testing.T) {
	for name, edit := range map[string][2]string{
		"unknown key":           {"triggerLabel: factory/ready", "triggerLabel: factory/ready\ntrigerLabel: x"},
		"run tokens > ceiling":  {"runTokens: 4000000", "runTokens: 6000000"},
		"text above the XRD":    {"maxTextBytes: 14336", "maxTextBytes: 32768"},
		"unknown role":          {"roles: [implementer]}", "roles: [implementor]}"},
		"missing tier":          {"  light:    {model: agent-default, runTokens: 300000,  taskTokens: 600000,  runMinutes: 20}\n", ""},
		"minutes above 480":     {"runMinutes: 90", "runMinutes: 600"},
		"default template gone": {"template: solo", "template: duo"},
		"no maintainer":         {"maintainers: [Smana]", "maintainers: []"},
		"bad duration":          {"issues: 60s", "issues: 60"},
		"unknown model":         {"model: agent-default, runTokens: 1500000", "model: gpt-5, runTokens: 1500000"},
		// Ruling SC: the broker's :8443 is TLS, verified against the mounted CA.
		"broker not https":    {`url: "https://room-broker`, `url: "http://room-broker`},
		"broker without a CA": {"caFile: /etc/agent-factory/openbao-ca/ca.crt, ", ""},
	} {
		t.Run(name, func(t *testing.T) {
			raw := strings.Replace(good, edit[0], edit[1], 1)
			if raw == good {
				t.Fatal("the edit did not apply")
			}
			if _, err := Parse([]byte(raw)); err == nil {
				t.Error("parsed")
			}
		})
	}
}
