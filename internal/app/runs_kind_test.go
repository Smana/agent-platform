// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/Smana/maku/internal/runwatch"
)

// Deleting runwatch.Configure from a startup path silently reverts a native deployment to
// the Crossplane XR. Both paths are driven until they fail later for want of a TLS pair (broker)
// or a CA file (factory), and the kind is read back.
const runsBlock = "runs:\n  group: maku.ogenki.io\n  namePrefix: run-\n"

var wantKind = runwatch.Kind{Group: "maku.ogenki.io", Namespace: "agents", NamePrefix: "run-"}

func writeCfg(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestEntrypointsConfigureTheRunsKind(t *testing.T) {
	logger := slog.New(slog.DiscardHandler)
	for _, tc := range []struct {
		name, wantErr string
		run           func(getenv func(string) string) error
		cfgEnv, cfg   string
		env           map[string]string
	}{
		{"serveBroker", "no TLS pair", func(g func(string) string) error { return serveBroker(context.Background(), logger, g) },
			"ROOMS_CONFIG", brokerCfg, map[string]string{"ROOMS_DATABASE_URL": "postgres://unused", "POD_NAMESPACE": "agent-platform"}},
		{"runFactory", "no broker CA", func(g func(string) string) error { return runFactory(context.Background(), logger, g) },
			"FACTORY_CONFIG", factoryCfg, map[string]string{"POD_NAMESPACE": "agent-system"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Cleanup(func() { _ = runwatch.Configure(runwatch.DefaultKind) })
			env := map[string]string{tc.cfgEnv: writeCfg(t, tc.cfg+runsBlock)}
			for k, v := range tc.env {
				env[k] = v
			}
			if err := tc.run(func(k string) string { return env[k] }); err == nil {
				t.Fatalf("%s: must fail after Configure", tc.wantErr)
			}
			if got := runwatch.Current(); got != wantKind {
				t.Fatalf("%s did not configure the runs kind: %+v", tc.name, got)
			}
		})
	}
}

const brokerCfg = `
publicURL: https://rooms.priv.aws.ogenki.io
runIssuers:
  - issuer: https://oidc.eks.eu-west-3.amazonaws.com/id/X
    jwksURL: https://oidc.eks.eu-west-3.amazonaws.com/id/X/keys
    subPattern: '^system:serviceaccount:agents:run-([a-z2-7]{8})$'
systemIssuer:
  issuer: https://oidc.eks.eu-west-3.amazonaws.com/id/X
  jwksURL: https://oidc.eks.eu-west-3.amazonaws.com/id/X/keys
systemPrincipals:
  system:serviceaccount:agent-system:agent-factory: system:factory
human:
  issuer: https://auth.example.test
  jwksURL: https://auth.example.test/oauth/v2/keys
  clientIDFile: /etc/room-broker/oidc/client-id
  projectIDFile: /etc/room-broker/oidc/project-id
  origin: https://rooms.example.test
  groups:
    admin: agents-admin
    member: agents-member
`

const factoryCfg = `
factory: aws-0
repository: Smana/cloud-native-ref
maintainers: [Smana]
triggerLabel: factory/ready
factoryLogin: ogenki-agent-factory[bot]
agentsLogin: ogenki-agents[bot]
roomsURL: https://rooms.priv.gcp.ogenki.io
broker: {url: "https://room-broker.agent-system.svc.cluster.local:8443", caFile: /etc/agent-factory/openbao-ca/ca.crt, tokenFile: /var/run/secrets/agents/rooms/token}
github: {appIDFile: /etc/agent-factory-github/app_id, privateKeyFile: /etc/agent-factory-github/private_key, mergerAppIDFile: /etc/agent-factory-merger/app_id, mergerKeyFile: /etc/agent-factory-merger/private_key}
poll: {issues: 60s, tasks: 30s, meter: 30s}
defaults: {template: solo, tier: standard, dataClass: public, predictedClass: review}
triage: {classifierURL: "http://complexity-classifier.agent-system.svc.cluster.local:8080/v1/classify", controlPercent: 10}
classes: {docs-links: {shadow: true}, revert: {shadow: true}, docs: {}, tests: {}, dashboards: {}}
merge:
  requiredChecks: ["Pre-commit checks 🛃", "Security scanning 🔒", "Kubernetes validation ☸", "Rendered manifest diff 📝", "Check the shell scripts 💻", "Check the documentation links 🔗", "Validate Vector Log Parsing Configuration (vlsingle)", "Validate Vector Log Parsing Configuration (vlcluster)"]
  verifyChecks: ["Pre-commit checks 🛃", "Security scanning 🔒", "Kubernetes validation ☸", "Check the shell scripts 💻", "Check the documentation links 🔗"]
  leakScanCheck: "Security scanning 🔒"
  policyBotLogin: ogenki-merge-gate[bot]
  mergerLogin: ogenki-agent-merger[bot]
  autoMergesPerDay: 10
  fixRuns: 2
  checkReruns: 1
  verifyFor: 30m
  revertWindow: 168h
  breaker: {window: 10, maxReverts: 1}
tiers:
  light:    {model: agent-default, runTokens: 300000,  taskTokens: 600000,  runMinutes: 20}
  standard: {model: agent-default, runTokens: 1500000, taskTokens: 3000000, runMinutes: 45}
  frontier: {model: agent-default, runTokens: 4000000, taskTokens: 8000000, runMinutes: 90}
templates:
  solo: {roles: [implementer]}
  pair: {roles: [implementer, reviewer], maxReviewRounds: 2}
  trio: {roles: [implementer, tester, reviewer], maxReviewRounds: 2}
  investigate: {roles: [triager]}  # R38: proposes a public issue text, never writes
caps: {activeTasks: 3, concurrentRuns: 4, tasksPerDay: 20, maxTextBytes: 14336, awaitingHumanWIP: 5}
budgets: {enforceTask: false, enforcePrincipal: false, factoryDailyUSD: 42.50, humanDailyUSD: 8.50}
api:
  listen: ":8443"
  repositories: [Smana/cloud-native-ref]
  humanIssuer: https://auth.ogenki.io
  humanJWKS: https://auth.ogenki.io/oauth/v2/keys
  clientIDFiles: [/etc/agent-factory-oidc/rooms-proxy-client-id, /etc/agent-factory-oidc/roomctl-client-id]
runlore:
  listen: ":8080"
  tokenFile: /etc/agent-factory-intake/token
  minConfidence: 0.75
  dailyCap: 5
meter:
  url: http://vmsingle-victoria-metrics-k8s-stack.observability.svc:8428
  query: 'sum by (ar_agent) (gen_ai_client_token_usage_sum{ar_agent=~"system:serviceaccount:agents:run-.+", gen_ai_token_type=~"input|output"})'
  logsURL: http://victoria-logs-victoria-logs-single-server.observability.svc:9428
  throttleQuery: '_time:2m kubernetes.pod_labels.gateway.envoyproxy.io/owning-gateway-name:"agent-router" | unpack_json | log.response_code:429 AND log.response_flags:~"RL" | stats by (log.x_ar_agent) count() hits'
`
