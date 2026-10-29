// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
)

// byModel is good priced by served model, as the host's meter is: every provider alias's pinned
// model has its row (v3 F15, F5).
var byModel = strings.Replace(strings.Replace(good, "sum by (ar_agent)", "sum by (ar_agent, gen_ai_request_model, gen_ai_token_type)", 1),
	"  logsURL:", "  prices: {glm-5.3: {input: 1.40, cacheRead: 0.26, output: 4.40}, k3.1-pro: {input: 2, cacheRead: 0.20, cacheWrite: 2, output: 10}, "+
		"mistral-medium-2604: {input: 1.50, cacheRead: 0.15, output: 7.50}, mistral-small-2603: {input: 0.15, cacheRead: 0.015, output: 0.60}}\n  logsURL:", 1)

// v3 F15, F3: loops.<class>.models picks a provider alias per role and tier, for public tasks
// only; a role or tier without an entry, and every internal task, keep today's model.
func TestModelFor(t *testing.T) {
	cfg, err := Parse([]byte(byModel + `loops:
  default: {models: {reviewer: {light: mistral-light}}}
  docs: {models: {implementer: {standard: mistral-standard, frontier: kimi-frontier}, triager: {light: mistral-light}}, judge: mistral-light}
`))
	if err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct{ class, dataClass, role, tier, want string }{
		"public docs, standard implementer": {"docs", "public", "implementer", "standard", "mistral-standard"},
		"public docs, frontier implementer": {"docs", "public", "implementer", "frontier", "kimi-frontier"},
		"public docs, the scoper's light":   {"docs", "public", "triager", "light", "mistral-light"},
		"internal docs: never an alias":     {"docs", "internal", "implementer", "standard", ""},
		"an unknown data class: never":      {"docs", "", "implementer", "standard", ""},
		"a tier without an entry":           {"docs", "public", "implementer", "light", ""},
		"a role without an entry":           {"docs", "public", "reviewer", "frontier", ""},
		"a row is whole: not default's":     {"docs", "public", "reviewer", "light", ""},
		"default's row reaches review":      {"review", "public", "reviewer", "light", "mistral-light"},
		"default's row, internal":           {"review", "internal", "reviewer", "light", ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := cfg.ModelFor(c.class, c.dataClass, c.role, c.tier); got != c.want {
				t.Fatalf("ModelFor = %q, want %q", got, c.want)
			}
		})
	}
	if cfg.LoopFor("default") != nil {
		t.Error("a models row switched the loop on")
	}
	if p := cfg.LoopFor("docs"); p == nil || p.JudgeModel != "mistral-light" {
		t.Errorf("docs' judge %+v, want mistral-light", p)
	}
	none, err := Parse([]byte(good))
	if err != nil {
		t.Fatal(err)
	}
	if got := none.ModelFor("docs", "public", "implementer", "standard"); got != "" {
		t.Errorf("no loops: %q, want today's model (D4)", got)
	}
}

func TestPublicOnly(t *testing.T) {
	for _, m := range []string{"kimi-frontier", "mistral-standard", "mistral-light"} {
		if !PublicOnly(m) {
			t.Errorf("%s is a public-only provider alias", m)
		}
	}
	for _, m := range []string{"", "agent-default", "tier-light", "tier-frontier", "k3.1-pro", "none"} {
		if PublicOnly(m) {
			t.Errorf("%q is not a provider alias", m)
		}
	}
}

// Config validation (F2, F3, F4): a key the factory cannot honour fails startup, and nothing that
// also reaches an internal task (a tier's model, a template's reviewerModel) may name a provider alias.
func TestProviderAliasesAreRefused(t *testing.T) {
	for name, c := range map[string]struct{ base, edit, want string }{
		"an unknown role": {byModel, "loops: {docs: {models: {merger: {standard: mistral-standard}}}}",
			`loops.docs.models: unknown role "merger"`},
		"an unknown tier": {byModel, "loops: {docs: {models: {implementer: {huge: mistral-standard}}}}",
			`loops.docs.models.implementer: "huge" is light, standard or frontier`},
		"another tier's alias": {byModel, "loops: {docs: {models: {implementer: {frontier: mistral-light}}}}",
			"loops.docs.models.implementer.frontier: mistral-light serves the light tier only"},
		"standard on frontier": {byModel, "loops: {docs: {models: {implementer: {frontier: mistral-standard}}}}",
			"loops.docs.models.implementer.frontier: mistral-standard serves the standard tier only"},
		"an unknown alias": {byModel, "loops: {docs: {models: {implementer: {standard: mistral-large}}}}",
			`loops.docs.models.implementer.standard: "mistral-large" is not a provider alias`},
		"a C5 name is not a provider alias": {byModel, "loops: {docs: {models: {implementer: {standard: tier-standard}}}}",
			`loops.docs.models.implementer.standard: "tier-standard" is not a provider alias`},
		"unpriced, prices by served model": {strings.Replace(byModel, "mistral-medium-2604: {input: 1.50, cacheRead: 0.15, output: 7.50}, ", "", 1),
			"loops: {docs: {models: {implementer: {standard: mistral-standard}}}}",
			"loops.docs.models.implementer.standard: mistral-standard needs meter.prices.mistral-medium-2604"},
		"unpriced, prices by logical model": {priced, "loops: {docs: {models: {implementer: {standard: mistral-standard}}}}",
			"loops.docs.models.implementer.standard: mistral-standard needs meter.prices.mistral-standard"},
		"an unknown class": {byModel, "loops: {nope: {models: {implementer: {standard: mistral-standard}}}}",
			`loops.nope: "nope" is not default, review or a class a repository declares`},
		"a tier's model": {strings.Replace(byModel, "light:    {model: agent-default", "light:    {model: mistral-light", 1), "",
			`tier light: model "mistral-light" is not a C5 logical name`},
		"a template's reviewerModel": {strings.Replace(byModel, "maxReviewRounds: 2}\n  trio", "maxReviewRounds: 2, reviewerModel: kimi-frontier}\n  trio", 1), "",
			`template pair: reviewerModel "kimi-frontier" is not a C5 logical name`},
		"a judge off the light tier": {byModel, "loops: {docs: {judge: kimi-frontier}}",
			`loops.docs.judge "kimi-frontier" is none or one of`},
		"an unpriced judge": {strings.Replace(byModel, "mistral-small-2603: {input: 0.15, cacheRead: 0.015, output: 0.60}", "", 1),
			"loops: {docs: {judge: mistral-light}}", "loops.docs.judge: mistral-light needs meter.prices.mistral-small-2603"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(c.base + c.edit + "\n"))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
	// Without meter.prices a tier keeps its token caps, and so does an alias: accepted.
	if _, err := Parse([]byte(good + "loops: {docs: {models: {implementer: {standard: mistral-standard}}, judge: mistral-light}}\n")); err != nil {
		t.Fatalf("no prices, token caps: %v", err)
	}
	// A price row under the alias itself is the logical-model meter's (a query not by model).
	aliasPriced := strings.Replace(priced, "  prices: {", "  prices: {mistral-standard: {input: 1.50, cacheRead: 0.15, output: 7.50}, ", 1)
	if _, err := Parse([]byte(aliasPriced + "loops: {docs: {models: {implementer: {standard: mistral-standard}}}}\n")); err != nil {
		t.Fatalf("priced under the alias: %v", err)
	}
}
