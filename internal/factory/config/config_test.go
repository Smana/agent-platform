// SPDX-License-Identifier: Apache-2.0

package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"

	"github.com/Smana/maku/internal/runwatch"
)

const good = `
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
	r := c.Repos()[0]
	if !r.IsMaintainer("smana") || r.IsMaintainer("someone") {
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
	if c.RunLore.Listen != ":8080" || c.RunLore.TokenFile != "/etc/agent-factory-intake/token" ||
		c.RunLore.MinConfidence != 0.75 || c.RunLore.DailyCap != 5 {
		t.Fatalf("runlore: %+v", c.RunLore)
	}
	if len(c.Hash) != 64 {
		t.Fatal("the config hash stamps each task (breaker)")
	}
	// §5.1 and §6.4: the arming gate's own block, and the classes held in shadow (R32).
	if len(r.Merge.RequiredChecks) != 8 || len(r.Merge.VerifyChecks) != 5 ||
		r.Merge.PolicyBotLogin != "ogenki-merge-gate[bot]" || r.Merge.MergerLogin != "ogenki-agent-merger[bot]" ||
		r.Merge.AutoMergesPerDay != 10 || r.Merge.FixRuns != 2 ||
		r.Merge.VerifyFor.Minutes() != 30 || r.Merge.RevertWindow.Hours() != 168 {
		t.Fatalf("merge: %+v", r.Merge)
	}
	if cl := r.Classes["docs-links"]; !cl.Shadow || cl.Live {
		t.Fatalf("docs-links is shadow until task 10.7, not live: %+v", cl)
	}
}

// OD-10: an omitted budgets block still carries the daily ceilings, the principal's unenforced
// (R3). The task cap is enforced unless the config says shadow (owner, 2026-10-07).
func TestBudgetDefaults(t *testing.T) {
	line := "budgets: {enforceTask: false, enforcePrincipal: false, factoryDailyUSD: 42.50, humanDailyUSD: 8.50}\n"
	for name, c := range map[string]struct {
		with    string
		enforce bool
	}{
		"omitted":             {"", true},
		"enforceTask omitted": {"budgets: {enforcePrincipal: false}\n", true},
		"shadow, written":     {line, false},
	} {
		t.Run(name, func(t *testing.T) {
			raw := strings.Replace(good, line, c.with, 1)
			if !strings.Contains(good, line) {
				t.Fatal("the edit did not apply")
			}
			cfg, err := Parse([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			if b := cfg.Budgets; b.FactoryDailyUSD != 42.50 || b.HumanDailyUSD != 8.50 || b.EnforceTask != c.enforce || b.EnforcePrincipal {
				t.Fatalf("defaults are $42.50 and $8.50, the principal's unenforced: %+v", b)
			}
		})
	}
}

// OD-6 (b): meter.prices turns every tier's run and task caps into USD, defaulting to their
// token caps in reference tokens; without prices a tier keeps its token caps, as before.
func TestUSDCaps(t *testing.T) {
	prices := "  prices: {agent-default: {input: 1.40, cacheRead: 0.26, output: 4.40}}\n  logsURL:"
	priced := func(s string) string { return strings.Replace(s, "  logsURL:", prices, 1) }
	for name, c := range map[string]struct {
		edit func(string) string
		want map[string][2]float64 // runUSD, taskUSD
	}{
		"no prices: token caps":   {func(s string) string { return s }, map[string][2]float64{"light": {0, 0}, "standard": {0, 0}, "frontier": {0, 0}}},
		"prices: the REF default": {priced, map[string][2]float64{"light": {0.51, 1.02}, "standard": {2.55, 5.10}, "frontier": {6.80, 13.60}}},
		"served-model prices, a query by model": {func(s string) string {
			s = strings.Replace(s, "sum by (ar_agent)", "sum by (ar_agent, gen_ai_request_model, gen_ai_token_type)", 1)
			return strings.Replace(s, "  logsURL:", "  prices: {glm-5.3: {input: 1.40, cacheRead: 0.26, output: 4.40}, claude-opus-5-5: {input: 4, cacheRead: 0.20, cacheWrite: 5, output: 20}}\n  logsURL:", 1)
		}, map[string][2]float64{"light": {0.51, 1.02}, "standard": {2.55, 5.10}, "frontier": {6.80, 13.60}}},
		"an explicit cap wins": {func(s string) string {
			return strings.Replace(priced(s), "runTokens: 4000000,", "runUSD: 10, taskUSD: 20, runTokens: 4000000,", 1)
		}, map[string][2]float64{"light": {0.51, 1.02}, "standard": {2.55, 5.10}, "frontier": {10, 20}}},
	} {
		t.Run(name, func(t *testing.T) {
			cfg, err := Parse([]byte(c.edit(good)))
			if err != nil {
				t.Fatal(err)
			}
			for tier, want := range c.want {
				if got := [2]float64{cfg.Tiers[tier].RunUSD, cfg.Tiers[tier].TaskUSD}; got != want {
					t.Errorf("tier %s: runUSD, taskUSD %v, want %v", tier, got, want)
				}
			}
		})
	}
}

func TestLoadReadsTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(good), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil || c.Repos()[0].Name != "Smana/cloud-native-ref" {
		t.Fatalf("%+v %v", c, err)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "absent.yaml")); err == nil {
		t.Fatal("a missing file fails startup")
	}
}

// awaitingCIFor off, written as 0 or left out, loads as no bound (#182).
func TestAwaitingCIForZeroOrOmittedIsOff(t *testing.T) {
	for name, raw := range map[string]string{
		"omitted": good,
		"zero":    strings.Replace(good, "revertWindow: 168h", "revertWindow: 168h\n  awaitingCIFor: 0s", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if name == "zero" && raw == good {
				t.Fatal("the edit did not apply")
			}
			c, err := Parse([]byte(raw))
			if err != nil {
				t.Fatal(err)
			}
			if d := c.Repos()[0].Merge.AwaitingCIFor.Duration; d != 0 {
				t.Fatalf("awaitingCIFor = %v", d)
			}
		})
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
		// R8: no C7 service, and the defaults the reconciler never reads, may all be left out.
		"no classifier URL":           {`classifierURL: "http://complexity-classifier.agent-system.svc.cluster.local:8080/v1/classify", `, ""},
		"no default template or tier": {"defaults: {template: solo, tier: standard, dataClass", "defaults: {dataClass"},
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

// #61: omitted is a week, an explicit 0 is no limit, anything else is itself, a negative fails.
func TestEscalatedHours(t *testing.T) {
	caps := "caps: {activeTasks: 3, concurrentRuns: 4, tasksPerDay: 20, maxTextBytes: 14336, awaitingHumanWIP: 5}"
	for name, c := range map[string]struct {
		with string
		want time.Duration
	}{
		"omitted":  {caps, 168 * time.Hour},
		"explicit": {strings.Replace(caps, "5}", "5, escalatedHours: 24}", 1), 24 * time.Hour},
		"zero":     {strings.Replace(caps, "5}", "5, escalatedHours: 0}", 1), 0},
	} {
		cfg, err := Parse([]byte(strings.Replace(good, caps, c.with, 1)))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := cfg.EscalatedLimit(); got != c.want {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
	if _, err := Parse([]byte(strings.Replace(good, caps, strings.Replace(caps, "5}", "5, escalatedHours: -1}", 1), 1))); err == nil ||
		!strings.Contains(err.Error(), "caps.escalatedHours") {
		t.Fatalf("a negative limit is refused: %v", err)
	}
}

// Disruption design §4: two automatic resumes per task unless the config says otherwise.
func TestResumeDefault(t *testing.T) {
	c, err := Parse([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if c.Resume.MaxPerTask != 2 {
		t.Fatalf("an omitted resume.maxPerTask is 2: %+v", c.Resume)
	}
}

// v2 suspend: runs are never made suspendable unless the file says so.
func TestSandboxSuspendIsOff(t *testing.T) {
	c, err := Parse([]byte(good))
	if err != nil || c.Sandbox.Suspend.Enabled {
		t.Fatalf("off by default: %+v, %v", c.Sandbox, err)
	}
	if c, err = Parse([]byte(good + "sandbox:\n  suspend:\n    enabled: true\n")); err != nil || !c.Sandbox.Suspend.Enabled {
		t.Fatalf("%+v, %v", c.Sandbox, err)
	}
	if _, err = Parse([]byte(good + "sandbox:\n  suspend:\n    after: 5m\n")); err == nil {
		t.Fatal("the idle threshold is the broker's, so the factory refuses it")
	}
}

// A bad config fails its rollout (§4): unknown keys, caps above the platform's, gaps. Each
// case names the error its own rule reports, so no rule hides behind another that fires on
// the same edit (review I2: a rule deleted must fail a test).
func TestBadConfigsFail(t *testing.T) {
	for name, c := range map[string]struct{ from, to, err string }{
		"no factory name":                {"factory: aws-0\n", "", "factory is required: the name repositories' .github/agent-factory.yaml give this factory (v2 D7)"},
		"a factory name not a DNS label": {"factory: aws-0", "factory: AWS_0", `factory "AWS_0" is not a DNS label`},
		"unknown key":                    {"triggerLabel: factory/ready", "triggerLabel: factory/ready\ntrigerLabel: x", `unknown field "trigerLabel"`},
		"run tokens > ceiling":           {"runTokens: 4000000", "runTokens: 6000000", "tier frontier: runTokens must be 1..5000000"},
		"run tokens below one":           {"runTokens: 300000,", "runTokens: 0,", "tier light: runTokens must be 1..5000000"},
		"task below run tokens":          {"taskTokens: 600000,", "taskTokens: 200000,", "tier light: taskTokens below runTokens"},
		"text above the XRD":             {"maxTextBytes: 14336", "maxTextBytes: 32768", "caps.maxTextBytes must be 1..14336"},
		"text below one byte":            {"maxTextBytes: 14336", "maxTextBytes: 0", "caps.maxTextBytes must be 1..14336"},
		// P: the Pending bound is 5..480 minutes; an omitted one defaults to 30.
		"pending minutes below five": {"maxTextBytes: 14336", "maxTextBytes: 14336, maxPendingMinutes: 4", "caps.maxPendingMinutes must be 5..480"},
		"pending minutes above 480":  {"maxTextBytes: 14336", "maxTextBytes: 14336, maxPendingMinutes: 481", "caps.maxPendingMinutes must be 5..480"},
		"resumes above five":         {"budgets: {enforceTask", "resume: {maxPerTask: 6}\nbudgets: {enforceTask", "resume.maxPerTask must be 1..5"},
		"negative resumes":           {"budgets: {enforceTask", "resume: {maxPerTask: -1}\nbudgets: {enforceTask", "resume.maxPerTask must be 1..5"},
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
		"control percent above 100":  {"controlPercent: 10", "controlPercent: 101", "triage.controlPercent must be 0..100"},
		"control percent negative":   {"controlPercent: 10", "controlPercent: -1", "triage.controlPercent must be 0..100"},
		"no docs-links class":        {"classes: {docs-links: {shadow: true}, revert: {shadow: true}", "classes: {revert: {shadow: true}", "class docs-links is required (OD-8)"},
		"no revert class":            {"classes: {docs-links: {shadow: true}, revert: {shadow: true}, ", "classes: {docs-links: {shadow: true}, ", "class revert is required (OD-8)"},
		"review declared as a class": {"classes: {docs-links:", "classes: {review: {live: true}, docs-links:", "review takes checks and testFirst only"},
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
		"negative check reruns":      {"checkReruns: 1", "checkReruns: -1", "merge.checkReruns must be 0 or more"},
		"verify for not positive":    {"verifyFor: 30m", "verifyFor: 0s", "merge.verifyFor must be positive"},
		"revert window not positive": {"revertWindow: 168h", "revertWindow: 0s", "merge.revertWindow must be positive"},
		"follow-ups over the window": {"breaker: {window: 10, maxReverts: 1}", "breaker: {window: 10, maxReverts: 1, maxFollowUps: 11}", "merge.breaker needs 0 ≤ maxFollowUps ≤ window (MC8)"},
		"follow-ups negative":        {"breaker: {window: 10, maxReverts: 1}", "breaker: {window: 10, maxReverts: 1, maxFollowUps: -1}", "merge.breaker needs 0 ≤ maxFollowUps ≤ window (MC8)"},
		"await ci bound negative":    {"revertWindow: 168h", "revertWindow: 168h\n  awaitingCIFor: -1m", "merge.awaitingCIFor must be 0 (no bound) or positive"},
		// R41 (review G5): the breaker's window must be able to trip: 1 ≤ maxReverts ≤ window.
		"breaker never trips":       {"maxReverts: 1}", "maxReverts: 0}", "merge.breaker needs 1 ≤ maxReverts ≤ window"},
		"breaker window 0":          {"breaker: {window: 10", "breaker: {window: 0", "merge.breaker needs 1 ≤ maxReverts ≤ window"},
		"no active tasks":           {"activeTasks: 3", "activeTasks: 0", "caps must be positive"},
		"no concurrent runs":        {"concurrentRuns: 4", "concurrentRuns: 0", "caps must be positive"},
		"no tasks per day":          {"tasksPerDay: 20", "tasksPerDay: 0", "caps must be positive"},
		"no awaiting human wip":     {"awaitingHumanWIP: 5", "awaitingHumanWIP: 0", "caps.awaitingHumanWIP must be positive"},
		"negative factory daily":    {"factoryDailyUSD: 42.50", "factoryDailyUSD: -1", "budgets.factoryDailyUSD must be a positive amount"},
		"negative human daily":      {"humanDailyUSD: 8.50", "humanDailyUSD: -1", "budgets.humanDailyUSD must be a positive amount"},
		"a day under a micro-USD":   {"humanDailyUSD: 8.50", "humanDailyUSD: 0.0000001", "budgets.humanDailyUSD must be a positive amount"},
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
		// FA-8 §1: every replica serves the RunLore intake, so its block is as required as api's.
		"no runlore listen":       {`listen: ":8080"`, `listen: ''`, "runlore.listen is required"},
		"no runlore token file":   {"tokenFile: /etc/agent-factory-intake/token", "tokenFile: ''", "runlore.tokenFile is required"},
		"runlore confidence high": {"minConfidence: 0.75", "minConfidence: 1.25", "runlore.minConfidence must be in (0, 1]"},
		"runlore confidence zero": {"minConfidence: 0.75", "minConfidence: 0", "runlore.minConfidence must be in (0, 1]"},
		"runlore cap negative":    {"dailyCap: 5", "dailyCap: -1", "runlore.dailyCap must be 0 or more"},
		// §1 schedules: the name keys the task, the cron must fire, the class must exist.
		"schedule name twice":       {"caps: {activeTasks: 3,", "schedules: [{name: a, cron: \"0 6 * * 1\", class: review, text: fix links}, {name: a, cron: \"0 7 * * 1\", class: review, text: renovate}]\ncaps: {activeTasks: 3,", `schedule name "a" is used twice`},
		"schedule name uppercase":   {"caps: {activeTasks: 3,", "schedules: [{name: Link-Rot, cron: \"0 6 * * 1\", class: review, text: fix}]\ncaps: {activeTasks: 3,", `must match ^[a-z0-9-]{1,40}$`},
		"schedule cron unparsable":  {"caps: {activeTasks: 3,", "schedules: [{name: a, cron: \"0 6 oops\", class: review, text: fix}]\ncaps: {activeTasks: 3,", `schedule a: cron "0 6 oops" does not parse`},
		"schedule class undeclared": {"caps: {activeTasks: 3,", "schedules: [{name: a, cron: \"0 6 * * 1\", class: ops, text: fix}]\ncaps: {activeTasks: 3,", `schedule a: class "ops" is review or a declared class`},
		"schedule text empty":       {"caps: {activeTasks: 3,", "schedules: [{name: a, cron: \"0 6 * * 1\", class: review, text: \"\"}]\ncaps: {activeTasks: 3,", "schedule a: text must be 1..14336 bytes"},
		"schedule probe unknown":    {"caps: {activeTasks: 3,", "schedules: [{name: a, cron: \"0 6 * * 1\", class: review, text: fix, probe: dependabot-red}]\ncaps: {activeTasks: 3,", `schedule a: probe "dependabot-red" is not renovate-red`},
		// A run cap in USD: priced, or the rollout fails (never an unpriced model metered at $0).
		"runUSD without a price": {"runTokens: 1500000,", "runUSD: 2.55, runTokens: 1500000,", "tier standard: runUSD and taskUSD need meter.prices.agent-default"},
		"negative taskUSD":       {"runTokens: 1500000,", "taskUSD: -1, runTokens: 1500000,", "tier standard: taskUSD must be a positive amount"},
		"taskUSD below runUSD":   {"runTokens: 1500000,", "runUSD: 3, taskUSD: 2, runTokens: 1500000,", "tier standard: taskUSD below runUSD"},
		"negative runUSD":        {"runTokens: 1500000,", "runUSD: -1, runTokens: 1500000,", "tier standard: runUSD must be a positive amount"},
		"a tier's model unpriced": {"  logsURL:", "  prices: {tier-light: {input: 1.40, cacheRead: 0.26, output: 4.40}}\n  logsURL:",
			"tier light: runUSD and taskUSD need meter.prices.agent-default"},
		"a price key that names no model": {"  logsURL:", "  prices: {agent-default: {input: 1.40, cacheRead: 0.26, output: 4.40}, 'GLM 5': {input: 1, cacheRead: 1, output: 1}}\n  logsURL:",
			`meter.prices: "GLM 5" is not a model name`},
		// A served model's price is read only from a query grouped by the model that served it.
		"a served model's price, a query not by model": {"  logsURL:", "  prices: {agent-default: {input: 1.40, cacheRead: 0.26, output: 4.40}, glm-5.3: {input: 1, cacheRead: 1, output: 1}}\n  logsURL:",
			`meter.prices.glm-5.3: a served model's price needs meter.query grouped by gen_ai_request_model`},
		"a price without its cache-read rate": {"  logsURL:", "  prices: {agent-default: {input: 1.40, output: 4.40}}\n  logsURL:",
			"meter.prices.agent-default: input, cacheRead and output must be positive"},
		"an unknown price field":      {"  logsURL:", "  prices: {agent-default: {input: 1.40, cached: 0.26, output: 4.40}}\n  logsURL:", `unknown field "cached"`},
		"schedule data class unknown": {"caps: {activeTasks: 3,", "schedules: [{name: a, cron: \"0 6 * * 1\", class: review, text: fix, dataClass: secret}]\ncaps: {activeTasks: 3,", `schedule a: dataClass "secret" is public or internal`},
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

// §1: schedules start tasks from config. The block parses, and a trusted entry carries its
// class, data class and probe.
func TestSchedules(t *testing.T) {
	raw := good + `schedules:
  - {name: link-rot, cron: "0 6 * * 1", class: docs-links, text: Fix broken external links.}
  - {name: renovate-red, cron: "0 6 * * 1", class: review, text: Fix red Renovate PRs., dataClass: internal, probe: renovate-red}
`
	c, err := Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	scheds := c.Repos()[0].Schedules
	if len(scheds) != 2 {
		t.Fatalf("%+v", scheds)
	}
	s := scheds[1]
	if s.Name != "renovate-red" || s.Cron != "0 6 * * 1" || s.Class != "review" || s.DataClass != "internal" || s.Probe != "renovate-red" {
		t.Fatalf("%+v", s)
	}
	if scheds[0].DataClass != "" || scheds[0].Probe != "" {
		t.Fatalf("an omitted dataClass falls back to the default at use; an omitted probe is none: %+v", scheds[0])
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

// Deployment facts come from the file; an omitted key keeps the platform's value, so a file
// that predates the keys loads unchanged and hashes as before.
func TestDeploymentFactsDefaultAndOverride(t *testing.T) {
	c, err := Parse([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if c.API.Groups.Admin != "agents-admin" || c.API.Groups.Member != "agents-member" {
		t.Fatalf("omitted api.groups default to the platform's: %+v", c.API.Groups)
	}
	raw := strings.Replace(good, "api:", "api:\n  groups: {admin: ops-admin, member: ops-member}", 1)
	raw += "schedules:\n  - {name: renovate-red, cron: \"0 6 * * 1\", class: review, text: Fix red PRs., probe: renovate-red}\n" +
		"  - {name: other, cron: \"0 6 * * 1\", class: review, text: Fix, probe: renovate-red, probeAuthor: \"mend[bot]\"}\n"
	if raw == good {
		t.Fatal("the edit did not apply")
	}
	o, err := Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	if o.API.Groups.Admin != "ops-admin" || o.API.Groups.Member != "ops-member" {
		t.Fatalf("api.groups: %+v", o.API.Groups)
	}
	if s := o.Repos()[0].Schedules; s[0].ProbeAuthor != "renovate[bot]" || s[1].ProbeAuthor != "mend[bot]" {
		t.Fatalf("probeAuthor defaults to renovate[bot]: %+v", s)
	}
}

func TestValidateNamesAMissingDeploymentFact(t *testing.T) {
	for name, edit := range map[string]struct {
		f   func(*Config)
		err string
	}{
		"member group": {func(c *Config) { c.API.Groups.Member = "" }, "api.groups.member is required"},
		"admin group":  {func(c *Config) { c.API.Groups.Admin = "" }, "api.groups.admin is required"},
		"probe author": {func(c *Config) { c.Repositories[0].Schedules[0].ProbeAuthor = "" }, "schedule a: probeAuthor is required"},
	} {
		t.Run(name, func(t *testing.T) {
			c, err := Parse([]byte(good + "schedules:\n  - {name: a, cron: \"0 6 * * 1\", class: review, text: Fix, probe: renovate-red}\n"))
			if err != nil {
				t.Fatal(err)
			}
			edit.f(c)
			if err := c.Validate(); err == nil || !strings.Contains(err.Error(), edit.err) {
				t.Errorf("err = %v, want one naming %q", err, edit.err)
			}
		})
	}
}

// A reviewer's model is chosen per template, apart from the tiers (default: the other tier's).
func TestAReviewerModelParses(t *testing.T) {
	c, err := Parse([]byte(strings.Replace(good, "maxReviewRounds: 2}\n  trio", "maxReviewRounds: 2, reviewerModel: tier-frontier}\n  trio", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if c.Templates["pair"].ReviewerModel != "tier-frontier" || c.Templates["trio"].ReviewerModel != "" {
		t.Fatalf("%+v", c.Templates)
	}
}

func TestRunsKind(t *testing.T) {
	c, err := Parse([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if c.Runs != runwatch.DefaultKind {
		t.Fatalf("no runs block: %+v", c.Runs)
	}
	c, err = Parse([]byte(good + "runs:\n  group: agents.ogenki.io\n  namePrefix: run-\n"))
	if err != nil || c.Runs.Group != "agents.ogenki.io" || c.Runs.Namespace != "agents" {
		t.Fatalf("runs %+v err %v", c.Runs, err)
	}
	if _, err := Parse([]byte(good + "runs:\n  group: nodot\n")); err == nil {
		t.Fatal("want an error for a group without a dot")
	}
}

// MC8: maxFollowUps parses; absent it counts none.
func TestMaxFollowUpsParses(t *testing.T) {
	c, err := Parse([]byte(strings.Replace(good, "breaker: {window: 10, maxReverts: 1}", "breaker: {window: 10, maxReverts: 1, maxFollowUps: 1}", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.RepositoryFor("Smana/cloud-native-ref").Merge.Breaker.MaxFollowUps; got != 1 {
		t.Fatalf("maxFollowUps = %d", got)
	}
	if c, err = Parse([]byte(good)); err != nil || c.RepositoryFor("Smana/cloud-native-ref").Merge.Breaker.MaxFollowUps != 0 {
		t.Fatalf("absent: %v", err)
	}
}

// R14: a config with no runlore block parses, the intake is off, and RunLoreRepository still
// answers the primary repository.
func TestRunLoreBlockIsOptional(t *testing.T) {
	block := "runlore:\n  listen: \":8080\"\n  tokenFile: /etc/agent-factory-intake/token\n  minConfidence: 0.75\n  dailyCap: 5\n"
	if !strings.Contains(good, block) {
		t.Fatal("the fixture's runlore block moved")
	}
	c, err := Parse([]byte(strings.Replace(good, block, "", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if c.RunLore.Listen != "" || c.RunLoreRepository() != c.Primary() {
		t.Fatalf("%+v %s", c.RunLore, c.RunLoreRepository())
	}
}

func TestMetricsMakuNames(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		want      bool
	}{
		{"unset is on", good, true},
		{"true is on", good + "metrics:\n  makuNames: true\n", true},
		{"false is off", good + "metrics:\n  makuNames: false\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Parse([]byte(tc.raw))
			if err != nil {
				t.Fatal(err)
			}
			if c.Metrics.MakuNames != tc.want {
				t.Fatalf("Metrics.MakuNames = %v, want %v", c.Metrics.MakuNames, tc.want)
			}
		})
	}
	if _, err := Parse([]byte(good + "metrics:\n  makuName: true\n")); err == nil {
		t.Fatal("a misspelt metrics key parsed")
	}
}
