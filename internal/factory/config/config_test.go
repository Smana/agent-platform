// SPDX-License-Identifier: Apache-2.0

package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

const good = `
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
budgets: {enforceTask: false, enforcePrincipal: false, factoryDaily: 25000000, humanDaily: 5000000}
api:
  listen: ":8443"
  repositories: [Smana/cloud-native-ref]
  humanIssuer: https://auth.ogenki.io
  humanJWKS: https://auth.ogenki.io/oauth/v2/keys
  clientIDFiles: [/etc/agent-factory-oidc/rooms-proxy-client-id, /etc/agent-factory-oidc/roomctl-client-id]
meter:
  url: http://vmsingle-victoria-metrics-k8s-stack.observability.svc:8428
  query: 'sum by (ar_agent) (gen_ai_client_token_usage_sum{ar_agent=~"system:serviceaccount:agents:xplane-run-.+", gen_ai_token_type=~"input|output"})'
  logsURL: http://victoria-logs-victoria-logs-single-server.observability.svc:9428
  throttleQuery: '_time:2m kubernetes.pod_labels.gateway.envoyproxy.io/owning-gateway-name:"agent-router" | unpack_json | log.response_code:429 AND log.response_flags:~"RL" | stats by (log.x_ar_agent) count() hits'
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
	if c.GitHub.MergerAppIDFile != "/etc/agent-factory-merger/app_id" || c.GitHub.MergerKeyFile != "/etc/agent-factory-merger/private_key" {
		t.Fatalf("github merger pair: %+v", c.GitHub)
	}
	if !c.IsMaintainer("smana") || c.IsMaintainer("someone") {
		t.Fatal("maintainers match case-insensitively, and only listed logins")
	}
	if c.Caps.MaxPendingMinutes != 30 {
		t.Fatalf("an omitted Pending bound defaults to 30 (P): %d", c.Caps.MaxPendingMinutes)
	}
	if c.Caps.AwaitingHumanWIP != 5 {
		t.Fatalf("awaitingHumanWIP: %d", c.Caps.AwaitingHumanWIP)
	}
	if c.API.Listen != ":8443" || len(c.API.Repositories) != 1 || len(c.API.ClientIDFiles) != 2 ||
		c.API.HumanIssuer != "https://auth.ogenki.io" || c.API.HumanJWKS != "https://auth.ogenki.io/oauth/v2/keys" {
		t.Fatalf("api: %+v", c.API)
	}
	if len(c.Hash) != 64 {
		t.Fatal("the config hash stamps each task (breaker)")
	}
	// §5.1 and §6.4: the arming gate's own block, and the classes held in shadow (R32).
	if len(c.Merge.RequiredChecks) != 8 || len(c.Merge.VerifyChecks) != 5 ||
		c.Merge.PolicyBotLogin != "ogenki-merge-gate[bot]" || c.Merge.MergerLogin != "ogenki-agent-merger[bot]" ||
		c.Merge.AutoMergesPerDay != 10 || c.Merge.FixRuns != 2 ||
		c.Merge.VerifyFor.Minutes() != 30 || c.Merge.RevertWindow.Hours() != 168 {
		t.Fatalf("merge: %+v", c.Merge)
	}
	if cl := c.Classes["docs-links"]; !cl.Shadow || cl.Live {
		t.Fatalf("docs-links is shadow until task 10.7, not live: %+v", cl)
	}
}

// OD-10: an omitted budgets block still carries the daily ceilings, unenforced (R3).
func TestBudgetDefaults(t *testing.T) {
	raw := strings.Replace(good, "budgets: {enforceTask: false, enforcePrincipal: false, factoryDaily: 25000000, humanDaily: 5000000}\n", "", 1)
	if raw == good {
		t.Fatal("the edit did not apply")
	}
	c, err := Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if c.Budgets.FactoryDaily != 25_000_000 || c.Budgets.HumanDaily != 5_000_000 || c.Budgets.EnforceTask || c.Budgets.EnforcePrincipal {
		t.Fatalf("defaults are 25 M and 5 M, both unenforced: %+v", c.Budgets)
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

// The template names are exactly the Task CRD's enum: a config can name no template a Task refuses.
func TestTemplatesAreTheTaskCRDEnum(t *testing.T) {
	raw, err := os.ReadFile("../../../config/crd/agents.ogenki.io_tasks.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatal(err)
	}
	var enum []string
	for _, v := range crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties["template"].Enum {
		var s string
		if err := json.Unmarshal(v.Raw, &s); err != nil {
			t.Fatal(err)
		}
		enum = append(enum, s)
	}
	if !slices.Equal(enum, templates()) {
		t.Fatalf("CRD %v, config %v", enum, templates())
	}
}

// The rules refuse only what they name: each variant here is a valid config.
func TestGoodVariantsParse(t *testing.T) {
	for name, c := range map[string][2]string{
		"internal data":        {"dataClass: public", "dataClass: internal"},
		"implementer anywhere": {"pair: {roles: [implementer, reviewer]", "pair: {roles: [reviewer, implementer]"},
		// P: an explicit Pending bound, inside 5..480.
		"an explicit pending bound": {"caps: {activeTasks: 3, concurrentRuns: 4, tasksPerDay: 20, maxTextBytes: 14336, awaitingHumanWIP: 5}",
			"caps: {activeTasks: 3, concurrentRuns: 4, tasksPerDay: 20, maxTextBytes: 14336, awaitingHumanWIP: 5, maxPendingMinutes: 20}"},
		// R23: a system caller pairs issuer with JWKS (empty today: none exists).
		"api with a system caller": {"humanJWKS: https://auth.ogenki.io/oauth/v2/keys",
			"humanJWKS: https://auth.ogenki.io/oauth/v2/keys\n  systemIssuer: https://accounts.cluster.local\n  systemJWKS: https://accounts.cluster.local/keys"},
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
		// P: the Pending bound is 5..480 minutes; an omitted one defaults to 30.
		"pending minutes below five": {"maxTextBytes: 14336", "maxTextBytes: 14336, maxPendingMinutes: 4", "caps.maxPendingMinutes must be 5..480"},
		"pending minutes above 480":  {"maxTextBytes: 14336", "maxTextBytes: 14336, maxPendingMinutes: 481", "caps.maxPendingMinutes must be 5..480"},
		"unknown role":               {"roles: [implementer]}", "roles: [implementor]}", `template solo: unknown role "implementor"`},
		"missing tier":               {"  light:    {model: agent-default, runTokens: 300000,  taskTokens: 600000,  runMinutes: 20}\n", "", "tier light is missing"},
		"a fourth tier": {"tiers:\n", "tiers:\n  huge: {model: agent-default, runTokens: 1, taskTokens: 1, runMinutes: 1}\n",
			"tiers are exactly light, standard and frontier"},
		"minutes above 480":     {"runMinutes: 90", "runMinutes: 600", "tier frontier: runMinutes must be 1..480"},
		"minutes below one":     {"runMinutes: 90", "runMinutes: 0", "tier frontier: runMinutes must be 1..480"},
		"default template gone": {"template: solo", "template: duo", `defaults.template "duo" is not a template`},
		"no maintainer":         {"maintainers: [Smana]", "maintainers: []", "maintainers is empty"},
		"a bot maintainer":      {"maintainers: [Smana]", `maintainers: [Smana, "renovate[bot]"]`, `maintainer "renovate[bot]" is an App`},
		"the factory App":       {"maintainers: [Smana]", `maintainers: ["Ogenki-Agent-Factory[BOT]"]`, `maintainer "Ogenki-Agent-Factory[BOT]" is an App`},
		"the factory App by name": {"maintainers: [Smana]\ntriggerLabel: factory/ready\nfactoryLogin: ogenki-agent-factory[bot]",
			"maintainers: [My-Factory]\ntriggerLabel: factory/ready\nfactoryLogin: my-factory", `maintainer "My-Factory" is an App`},
		"the agents App by name": {"maintainers: [Smana]\ntriggerLabel: factory/ready\nfactoryLogin: ogenki-agent-factory[bot]\nagentsLogin: ogenki-agents[bot]",
			"maintainers: [my-agents]\ntriggerLabel: factory/ready\nfactoryLogin: ogenki-agent-factory[bot]\nagentsLogin: my-agents", `maintainer "my-agents" is an App`},
		"the agents App, bare": {"maintainers: [Smana]", "maintainers: [ogenki-agents]", `maintainer "ogenki-agents" is an App`},
		"bad duration":         {"issues: 60s", "issues: 60", "a duration is a string"},
		"unknown model":        {"model: agent-default, runTokens: 1500000", "model: gpt-5, runTokens: 1500000", `tier standard: model "gpt-5" is not a C5 logical name`},
		// Ruling SI: the reconciler creates Tasks from these names, and the Task CRD's enum refuses others.
		"a template off the CRD enum": {"  solo: {roles: [implementer]}\n", "  solo: {roles: [implementer]}\n  duo: {roles: [implementer]}\n",
			"template duo is not one of solo, pair, trio, investigate"},
		"template without roles": {"solo: {roles: [implementer]}", "solo: {roles: []}", "template solo has no roles"},
		// R38 (owner default): only a lone triager may go without an implementer.
		"a reviewer without an implementer": {"investigate: {roles: [triager]}", "investigate: {roles: [reviewer]}",
			"template investigate: only an implementer writes"},
		"a triager with a reviewer": {"investigate: {roles: [triager]}", "investigate: {roles: [triager, reviewer]}",
			"template investigate: only an implementer writes"},
		"unknown default tier": {"tier: standard,", "tier: medium,", `defaults.tier "medium" is not a tier`},
		"unknown data class":   {"dataClass: public", "dataClass: secret", "defaults.dataClass is public or internal"},
		"no predicted class":   {", predictedClass: review}", "}", "defaults.predictedClass is required"},
		// §5.2: C7's endpoint, the OD-14 share, and the two entry classes are required.
		"no classifier URL":          {`classifierURL: "http://complexity-classifier.agent-system.svc.cluster.local:8080/v1/classify", `, "", "triage.classifierURL is required"},
		"control percent above 100":  {"controlPercent: 10", "controlPercent: 101", "triage.controlPercent must be 0..100"},
		"control percent negative":   {"controlPercent: 10", "controlPercent: -1", "triage.controlPercent must be 0..100"},
		"no docs-links class":        {"classes: {docs-links: {shadow: true}, revert: {shadow: true}", "classes: {revert: {shadow: true}", "class docs-links is required (OD-8)"},
		"no revert class":            {"classes: {docs-links: {shadow: true}, revert: {shadow: true}, ", "classes: {docs-links: {shadow: true}, ", "class revert is required (OD-8)"},
		"review declared as a class": {"classes: {docs-links:", "classes: {review: {}, docs-links:", "review is the implicit class of everything else"},
		// R32: a class is decided live or held in shadow, never both.
		"live and shadow": {"docs-links: {shadow: true}", "docs-links: {live: true, shadow: true}", "class docs-links: live and shadow never together"},
		// §5.1's arming and §6.4's rollback: the merge block is the gate's own config.
		"merge without required checks": {"requiredChecks: [\"Pre-commit checks 🛃\", \"Security scanning 🔒\", \"Kubernetes validation ☸\", \"Rendered manifest diff 📝\", \"Check the shell scripts 💻\", \"Check the documentation links 🔗\", \"Validate Vector Log Parsing Configuration (vlsingle)\", \"Validate Vector Log Parsing Configuration (vlcluster)\"]",
			"requiredChecks: []", "merge.requiredChecks is empty"},
		"merge without verify checks": {"verifyChecks: [\"Pre-commit checks 🛃\", \"Security scanning 🔒\", \"Kubernetes validation ☸\", \"Check the shell scripts 💻\", \"Check the documentation links 🔗\"]",
			"verifyChecks: []", "merge.verifyChecks is empty"},
		// R42 (review G6): the secret scan cannot be dropped from either list silently.
		"leak scan not required":     {"leakScanCheck: \"Security scanning 🔒\"", "leakScanCheck: \"Trivy\"", "merge.leakScanCheck"},
		"no leak scan":               {"  leakScanCheck: \"Security scanning 🔒\"\n", "", "merge.leakScanCheck"},
		"policy bot not a bot":       {"policyBotLogin: ogenki-merge-gate[bot]", "policyBotLogin: ogenki-merge-gate", "merge.policyBotLogin"},
		"merger not a bot":           {"mergerLogin: ogenki-agent-merger[bot]", "mergerLogin: ogenki-agent-merger", "merge.mergerLogin"},
		"negative auto merges":       {"autoMergesPerDay: 10", "autoMergesPerDay: -1", "merge.autoMergesPerDay must be 0 or more"},
		"negative fix runs":          {"fixRuns: 2", "fixRuns: -1", "merge.fixRuns must be 0 or more"},
		"verify for not positive":    {"verifyFor: 30m", "verifyFor: 0s", "merge.verifyFor must be positive"},
		"revert window not positive": {"revertWindow: 168h", "revertWindow: 0s", "merge.revertWindow must be positive"},
		// R41 (review G5): the breaker's window must be able to trip: 1 ≤ maxReverts ≤ window.
		"breaker never trips":       {"maxReverts: 1}", "maxReverts: 0}", "merge.breaker needs 1 ≤ maxReverts ≤ window"},
		"breaker window 0":          {"breaker: {window: 10", "breaker: {window: 0", "merge.breaker needs 1 ≤ maxReverts ≤ window"},
		"no active tasks":           {"activeTasks: 3", "activeTasks: 0", "caps must be positive"},
		"no concurrent runs":        {"concurrentRuns: 4", "concurrentRuns: 0", "caps must be positive"},
		"no tasks per day":          {"tasksPerDay: 20", "tasksPerDay: 0", "caps must be positive"},
		"no awaiting human wip":     {"awaitingHumanWIP: 5", "awaitingHumanWIP: 0", "caps.awaitingHumanWIP must be positive"},
		"negative factory daily":    {"factoryDaily: 25000000", "factoryDaily: -1", "budgets.factoryDaily must be positive"},
		"negative human daily":      {"humanDaily: 5000000", "humanDaily: -1", "budgets.humanDaily must be positive"},
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
		// Narration posts it on public issues as the watch link: the tailnet UI over TLS, nothing else.
		"rooms URL not https":       {"roomsURL: https://rooms", "roomsURL: http://rooms", "roomsURL must be https://<host>"},
		"rooms URL without host":    {"roomsURL: https://rooms.priv.gcp.ogenki.io", "roomsURL: https://", "roomsURL must be https://<host>"},
		"rooms URL with userinfo":   {"roomsURL: https://rooms", "roomsURL: https://u:p@rooms", "roomsURL must be https://<host>"},
		"rooms URL with a query":    {"roomsURL: https://rooms.priv.gcp.ogenki.io", "roomsURL: https://rooms.priv.gcp.ogenki.io/?x=1", "roomsURL must be https://<host>"},
		"rooms URL with a path":     {"roomsURL: https://rooms.priv.gcp.ogenki.io", "roomsURL: https://rooms.priv.gcp.ogenki.io/r", "roomsURL must be https://<host>"},
		"rooms URL with markdown":   {"roomsURL: https://rooms.priv.gcp.ogenki.io", "roomsURL: 'https://rooms.priv.gcp.ogenki.io)'", "roomsURL must be https://<host>"},
		"rooms URL with a fragment": {"roomsURL: https://rooms.priv.gcp.ogenki.io", "roomsURL: 'https://rooms.priv.gcp.ogenki.io#x'", "roomsURL must be https://<host>"},
		"rooms URL with a tag":      {"roomsURL: https://rooms.priv.gcp.ogenki.io", "roomsURL: 'https://ro<img>ms'", "roomsURL must be https://<host>"},
		"no app id file":            {"appIDFile: /etc/agent-factory-github/app_id", "appIDFile: ''", "github.appIDFile is required"},
		"no private key file":       {"privateKeyFile: /etc/agent-factory-github/private_key", "privateKeyFile: ''", "github.privateKeyFile is required"},
		// R16: the merger App's key is both files or neither, and a live or shadow class reaches
		// for it: such a config without the pair fails its rollout.
		"a shadow class without the merger key": {", mergerAppIDFile: /etc/agent-factory-merger/app_id, mergerKeyFile: /etc/agent-factory-merger/private_key",
			"", "github.mergerAppIDFile: required by a live or shadow class"},
		"half a merger key pair": {"mergerAppIDFile: /etc/agent-factory-merger/app_id, mergerKeyFile: /etc/agent-factory-merger/private_key",
			"mergerAppIDFile: /etc/agent-factory-merger/app_id", "github.mergerAppIDFile and github.mergerKeyFile are both set or both empty"},
		"no meter URL": {"url: http://vmsingle-victoria-metrics-k8s-stack.observability.svc:8428", "url: ''", "meter.url is required"},
		"no meter query": {`query: 'sum by (ar_agent) (gen_ai_client_token_usage_sum{ar_agent=~"system:serviceaccount:agents:xplane-run-.+", gen_ai_token_type=~"input|output"})'`,
			"query: ''", "meter.query is required"},
		// R13: the 429 lookup needs its endpoint and its query.
		"no meter logs URL": {"logsURL: http://victoria-logs-victoria-logs-single-server.observability.svc:9428", "logsURL: ''", "meter.logsURL is required"},
		"no meter throttle query": {`throttleQuery: '_time:2m kubernetes.pod_labels.gateway.envoyproxy.io/owning-gateway-name:"agent-router" | unpack_json | log.response_code:429 AND log.response_flags:~"RL" | stats by (log.x_ar_agent) count() hits'`,
			"throttleQuery: ''", "meter.throttleQuery is required"},
		// §4: what the run-request API binds, admits and authenticates against.
		"no api listen":                 {`listen: ":8443"`, `listen: ''`, "api.listen is required"},
		"no api issuer":                 {"humanIssuer: https://auth.ogenki.io", "humanIssuer: ''", "api.humanIssuer is required"},
		"no api jwks":                   {"humanJWKS: https://auth.ogenki.io/oauth/v2/keys", "humanJWKS: ''", "api.humanJWKS is required"},
		"api without repositories":      {"repositories: [Smana/cloud-native-ref]", "repositories: []", "api.repositories is empty"},
		"api repository not owner/name": {"repositories: [Smana/cloud-native-ref]", "repositories: ['../x']", `api.repository "../x" is not owner/name`},
		"api without client ids":        {"clientIDFiles: [/etc/agent-factory-oidc/rooms-proxy-client-id, /etc/agent-factory-oidc/roomctl-client-id]", "clientIDFiles: []", "api.clientIDFiles is empty"},
		"api system issuer alone":       {"humanIssuer: https://auth.ogenki.io", "humanIssuer: https://auth.ogenki.io\n  systemIssuer: https://accounts.example", "api.systemIssuer and api.systemJWKS are both set or both empty"},
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

// R16: the merger pair binds to live or shadow classes only: an all-prediction config needs
// no merger key, but dropping one half of a configured pair is refused by the pair rule.
func TestMergerPairFollowsClasses(t *testing.T) {
	raw := strings.Replace(good, ", mergerAppIDFile: /etc/agent-factory-merger/app_id, mergerKeyFile: /etc/agent-factory-merger/private_key", "", 1)
	raw = strings.Replace(raw, "classes: {docs-links: {shadow: true}, revert: {shadow: true},", "classes: {docs-links: {}, revert: {},", 1)
	if raw == good {
		t.Fatal("the edits did not apply")
	}
	if _, err := Parse([]byte(raw)); err != nil {
		t.Fatalf("no live or shadow class, no pair: %v", err)
	}
	if _, err := Parse([]byte(strings.Replace(raw, "classes: {docs-links: {},", "classes: {docs-links: {live: true},", 1))); err == nil {
		t.Fatal("a live class without the pair is refused")
	}
}

// R46: task spans go to the trace collector's platform port, host:port; no block, no tracing.
func TestTracingEndpoint(t *testing.T) {
	c, err := Parse([]byte(good + "tracing: {otlpEndpoint: agent-traces-collector.observability.svc.cluster.local:4317}\n"))
	if err != nil || c.Tracing.OTLPEndpoint != "agent-traces-collector.observability.svc.cluster.local:4317" {
		t.Fatalf("%v %+v", err, c)
	}
	for _, bad := range []string{`"http://collector:4317"`, "collector", "collector:", ":4317", "-collector:4317", "Collector:4317", "collector:123456"} {
		if _, err := Parse([]byte(good + "tracing: {otlpEndpoint: " + bad + "}\n")); err == nil {
			t.Errorf("%s is not host:port", bad)
		}
	}
	if c, err := Parse([]byte(good)); err != nil || c.Tracing.OTLPEndpoint != "" {
		t.Fatal("no tracing block: tracing off")
	}
}
