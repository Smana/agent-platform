// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
)

// priced is good with a price row: the tiers' USD caps default from their token caps (TC5), as the
// host's do: standard $2.55/$5.10.
var priced = strings.Replace(good, "  logsURL:",
	"  prices: {agent-default: {input: 1.40, cacheRead: 0.26, output: 4.40}}\n  logsURL:", 1)

// Splitter design SL3, offline proof 1: the split parses and resolves its defaults; it is absent by
// default; default's row reaches review but never docs-links or revert.
func TestSplitFor(t *testing.T) {
	cfg, err := Parse([]byte(priced + `loops:
  default: {scope: scoper, split: {maxChildren: 2}}
  docs: {scope: scoper, split: {maxChildren: 4}}
  tests: {scope: scoper, split: {maxChildren: 3, familyUSD: 12, childTier: light}}
  dashboards: {scope: scoper}
`))
	if err != nil {
		t.Fatal(err)
	}
	const repo = "Smana/cloud-native-ref"
	for class, want := range map[string]*SplitPolicy{
		"docs":       {MaxChildren: 4, FamilyUSD: 20.4, ChildTier: "standard"}, // SP3, SP5
		"tests":      {MaxChildren: 3, FamilyUSD: 12, ChildTier: "light"},
		"review":     {MaxChildren: 2, FamilyUSD: 20.4, ChildTier: "standard"},
		"dashboards": nil, // a row is whole: no split of its own
		"docs-links": nil, // default's row never splits it (SL3)
		"revert":     nil,
	} {
		got := cfg.SplitFor(repo, class)
		if (got == nil) != (want == nil) || (got != nil && (got.MaxChildren != want.MaxChildren || got.ChildTier != want.ChildTier ||
			MicroUSD(got.FamilyUSD) != MicroUSD(want.FamilyUSD))) {
			t.Errorf("SplitFor(%s) = %+v, want %+v", class, got, want)
		}
	}
	if cfg.LoopFor("docs") != nil {
		t.Error("a split row switched the loop on")
	}
	none, err := Parse([]byte(priced))
	if err != nil {
		t.Fatal(err)
	}
	if none.SplitFor(repo, "docs") != nil {
		t.Error("no loops: a split is offered (D4)")
	}
}

func TestSplitsAreRefused(t *testing.T) {
	oracle := strings.Replace(priced, "dashboards: {}}", `dashboards: {}, links: {oracle: {probe: dead-links, workflow: links.yaml, maxFindings: 5}, `+
		`holdoutChecks: [External links], diffShape: links-only, modifyOnly: true, allowedFiles: ["^docs/.+[.]md$"]}}`, 1)
	for name, c := range map[string]struct{ base, loops, want string }{
		"without the scoper":    {priced, "{docs: {scope: steering, split: {maxChildren: 3}}}", "loops.docs.split needs scope: scoper on the same row"},
		"docs-links":            {priced, "{docs-links: {scope: scoper, split: {maxChildren: 3}}}", "loops.docs-links.split: docs-links tasks are never split"},
		"revert":                {priced, "{revert: {scope: scoper, split: {maxChildren: 3}}}", "loops.revert.split: revert tasks are never split"},
		"an oracle class":       {oracle, "{links: {scope: scoper, split: {maxChildren: 3}}}", "loops.links.split: links tasks are never split"},
		"maxChildren 6":         {priced, "{docs: {scope: scoper, split: {maxChildren: 6}}}", "loops.docs.split.maxChildren must be 2..5"},
		"no maxChildren":        {priced, "{docs: {scope: scoper, split: {}}}", "loops.docs.split.maxChildren must be 2..5"},
		"an unknown child tier": {priced, "{docs: {scope: scoper, split: {maxChildren: 3, childTier: huge}}}", `childTier "huge" is light, standard or frontier`},
		"below the child tier":  {priced, "{docs: {scope: scoper, split: {maxChildren: 3, familyUSD: 5}}}", "familyUSD 5.00 is below the standard tier's taskUSD 5.10"},
		"no prices":             {good, "{docs: {scope: scoper, split: {maxChildren: 3}}}", "loops.docs.split needs meter.prices"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(c.base + "loops: " + c.loops + "\n"))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
}
