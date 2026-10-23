// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
	"time"
)

const classesLine = "classes: {docs-links: {shadow: true}, revert: {shadow: true}, docs: {}, tests: {}, dashboards: {}}"

const deadLinks = `dead-links: {allowedFiles: ['^(docs|website/content)/.+\.md$'], protectedFiles: ['^docs/(superpowers|specs)/'],
    modifyOnly: true, maxPatchFiles: 3, maxModifiedLines: 20, diffShape: links-only, holdoutChecks: ["External links 🔗"], maxPerDay: 1}`

func withClasses(t *testing.T, classes string) string {
	t.Helper()
	if !strings.Contains(good, classesLine) {
		t.Fatal("the fixture's classes line moved")
	}
	return strings.Replace(good, classesLine, "classes:\n  "+classes, 1)
}

// MC1: a class declares its conditions by name; absent, it is decided as before.
func TestDeclaredConditionsParse(t *testing.T) {
	c, err := Parse([]byte(withClasses(t, "docs-links: {shadow: true}\n  revert: {shadow: true}\n  "+deadLinks)))
	if err != nil {
		t.Fatal(err)
	}
	cl := c.RepositoryFor("Smana/cloud-native-ref").Classes["dead-links"]
	if !cl.ModifyOnly || cl.MaxModifiedLines != 20 || cl.DiffShape != "links-only" || cl.HoldoutChecks[0] != "External links 🔗" || cl.Live || cl.Shadow {
		t.Fatalf("%+v", cl)
	}
	if len(cl.AllowedFiles) != 1 || cl.AllowedFiles[0] != `^(docs|website/content)/.+\.md$` || len(cl.ProtectedFiles) != 1 || cl.ProtectedFiles[0] != `^docs/(superpowers|specs)/` ||
		cl.MaxPatchFiles != 3 || cl.MaxPerDay != 1 {
		t.Fatalf("allowed, protected, maxPatchFiles or maxPerDay decoded into the wrong field: %+v", cl)
	}
	if !cl.ReadsDiff("dead-links") || !(Class{}).ReadsDiff("docs-links") || (Class{Shadow: true}).ReadsDiff("docs") {
		t.Fatal("ReadsDiff: a declared file condition or a built-in shape reads the diff, nothing else does")
	}
}

// Offline proof 3: an unknown condition fails the strict decoder; a bad value fails Validate.
func TestBadConditionsFail(t *testing.T) {
	for name, c := range map[string]struct{ classes, err string }{
		"gh-aw's spelling":    {"docs-links: {live: true, max-patch-size: 3}\n  revert: {}", `unknown field "max-patch-size"`},
		"an unknown key":      {"docs-links: {live: true, maxPatchSize: 3}\n  revert: {}", `unknown field "maxPatchSize"`},
		"an unanchored allow": {"docs-links: {}\n  revert: {}\n  x: {allowedFiles: ['^docs/.+']}", `class x: allowedFiles "^docs/.+" must start with ^ and end with $`},
		"an unanchored start": {"docs-links: {}\n  revert: {}\n  x: {allowedFiles: ['docs/.+$']}", `class x: allowedFiles "docs/.+$" must start with ^ and end with $`},
		"a bad pattern":       {"docs-links: {}\n  revert: {}\n  x: {allowedFiles: ['(']}", `class x: allowedFiles "(" is not a regular expression`},
		"an empty pattern":    {"docs-links: {}\n  revert: {}\n  x: {protectedFiles: ['']}", `class x: protectedFiles "" is not a regular expression`},
		"a negative cap":      {"docs-links: {}\n  revert: {}\n  x: {maxModifiedLines: -1}", "class x: maxModifiedLines must be 0 (no cap) or more"},
		"an unknown shape":    {"docs-links: {}\n  revert: {}\n  x: {diffShape: semver-major}", `class x: diffShape "semver-major" is not one of [links-only semver-patch semver-minor]`},
		"a holdout twice":     {"docs-links: {}\n  revert: {}\n  x: {holdoutChecks: [a, a]}", `class x: holdoutChecks names "a" empty or twice`},
		"live and shadow":     {"docs-links: {live: true, shadow: true}\n  revert: {}", "class docs-links: live and shadow never together (R32)"},
		"a negative per day":  {"docs-links: {}\n  revert: {}\n  x: {maxPerDay: -2}", "class x: maxPerDay must be 0 (no cap) or more"},
		"an oracle without a holdout": {"docs-links: {}\n  revert: {}\n  x: {diffShape: links-only, oracle: {probe: dead-links, workflow: dead-links.yaml, maxFindings: 10}}",
			"class x: an oracle needs holdoutChecks, or the head goes unproven (MC5)"},
		"an oracle off links-only": {"docs-links: {}\n  revert: {}\n  x: {holdoutChecks: [h], oracle: {probe: dead-links, workflow: dead-links.yaml, maxFindings: 10}}",
			"class x: an oracle reads link targets: diffShape must be links-only"},
		"too many findings": {"docs-links: {}\n  revert: {}\n  x: {diffShape: links-only, holdoutChecks: [h], oracle: {probe: dead-links, workflow: dead-links.yaml, maxFindings: 17}}",
			"class x: oracle.maxFindings must be 1..16"},
		"an oracle without modifyOnly": {"docs-links: {}\n  revert: {}\n  x: {diffShape: links-only, holdoutChecks: [h], allowedFiles: ['^docs/.+$'], oracle: {probe: dead-links, workflow: dead-links.yaml, maxFindings: 10}}",
			"class x: an oracle needs modifyOnly: true, or an added or deleted empty file would pass"},
		"an oracle without allowedFiles": {"docs-links: {}\n  revert: {}\n  x: {diffShape: links-only, holdoutChecks: [h], modifyOnly: true, oracle: {probe: dead-links, workflow: dead-links.yaml, maxFindings: 10}}",
			"class x: an oracle needs allowedFiles, or the probe drops every finding"},
		"an oracle with a patch too small": {"docs-links: {}\n  revert: {}\n  x: {diffShape: links-only, holdoutChecks: [h], modifyOnly: true, allowedFiles: ['^docs/.+$'], maxModifiedLines: 19, oracle: {probe: dead-links, workflow: dead-links.yaml, maxFindings: 10}}",
			"class x: maxModifiedLines 19 is under 2 x oracle.maxFindings (20): one retarget changes two lines"},
		"an unknown probe": {"docs-links: {}\n  revert: {}\n  x: {diffShape: links-only, holdoutChecks: [h], oracle: {probe: trivy, workflow: t.yaml, maxFindings: 1}}",
			`class x: oracle.probe "trivy" is not dead-links`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(withClasses(t, c.classes)))
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Fatalf("got %v, want %q", err, c.err)
			}
		})
	}
}

// A complete oracle class passes, with a patch limit that holds every retarget.
func TestAnOracleClassWithItsConditionsIsValid(t *testing.T) {
	c := "docs-links: {}\n  revert: {}\n  x: {diffShape: links-only, holdoutChecks: [h], modifyOnly: true, allowedFiles: ['^docs/.+$'], maxModifiedLines: 20, oracle: {probe: dead-links, workflow: dead-links.yaml, maxFindings: 10}}"
	if _, err := Parse([]byte(withClasses(t, c))); err != nil {
		t.Fatal(err)
	}
}

// MC7, MC8: a class declares its monitor; the breaker counts follow-ups when maxFollowUps > 0.
func TestMonitorParses(t *testing.T) {
	c, err := Parse([]byte(withClasses(t, "docs-links: {}\n  revert: {}\n  "+deadLinks[:len(deadLinks)-1]+
		", oracle: {probe: dead-links, workflow: dead-links.yaml, maxFindings: 10}, modifyOnly: true,"+
		" monitor: {oracleRuns: 2, followUpWindow: 168h, flux: {source: flux-system/flux-system, within: 1h}}}")))
	if err != nil {
		t.Fatal(err)
	}
	m := c.RepositoryFor("Smana/cloud-native-ref").Classes["dead-links"].Monitor
	if m == nil || m.OracleRuns != 2 || m.FollowUpWindow.Duration != 168*time.Hour || m.Flux.Namespace() != "flux-system" ||
		m.Flux.Name() != "flux-system" || m.Flux.Within.Duration != time.Hour {
		t.Fatalf("%+v", m)
	}
}

func TestBadMonitorFails(t *testing.T) {
	for name, c := range map[string]struct{ classes, err string }{
		"no window":              {"docs-links: {}\n  revert: {}\n  x: {monitor: {}}", "class x: monitor.followUpWindow must be 1h..720h"},
		"oracle runs, no oracle": {"docs-links: {}\n  revert: {}\n  x: {monitor: {oracleRuns: 2, followUpWindow: 168h}}", "class x: monitor.oracleRuns needs an oracle"},
		"too many runs":          {"docs-links: {}\n  revert: {}\n  x: {monitor: {oracleRuns: 9, followUpWindow: 168h}}", "class x: monitor.oracleRuns must be 0..5"},
		"a bad source":           {"docs-links: {}\n  revert: {}\n  x: {monitor: {followUpWindow: 168h, flux: {source: flux-system, within: 1h}}}", `class x: monitor.flux.source "flux-system" is not namespace/name`},
		"no within":              {"docs-links: {}\n  revert: {}\n  x: {monitor: {followUpWindow: 168h, flux: {source: a/b}}}", "class x: monitor.flux.within must be 5m..6h"},
		"an unknown key":         {"docs-links: {}\n  revert: {}\n  x: {monitor: {followUps: 1}}", `unknown field "followUps"`},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(withClasses(t, c.classes))); err == nil || !strings.Contains(err.Error(), c.err) {
				t.Fatalf("got %v, want %q", err, c.err)
			}
		})
	}
}

// B1: a monitored class needs the merger's label permission even while no class is live.
func TestAnyMonitored(t *testing.T) {
	c, err := Parse([]byte(withClasses(t, "docs-links: {}\n  revert: {}\n  x: {monitor: {followUpWindow: 168h}}")))
	if err != nil {
		t.Fatal(err)
	}
	if !c.AnyMonitored() || c.AnyLive() {
		t.Fatalf("monitored %v, live %v", c.AnyMonitored(), c.AnyLive())
	}
	if c, err = Parse([]byte(withClasses(t, "docs-links: {}\n  revert: {}"))); err != nil || c.AnyMonitored() {
		t.Fatalf("no monitor declared: %v %v", c.AnyMonitored(), err)
	}
}

// The Flux reader is built only when a class declares monitor.flux.
func TestAnyFlux(t *testing.T) {
	c, err := Parse([]byte(withClasses(t, "docs-links: {}\n  revert: {}\n  x: {monitor: {followUpWindow: 168h, flux: {source: flux-system/flux-system, within: 1h}}}")))
	if err != nil || !c.AnyFlux() {
		t.Fatalf("flux declared: %v %v", c.AnyFlux(), err)
	}
	if c, err = Parse([]byte(withClasses(t, "docs-links: {}\n  revert: {}\n  x: {monitor: {followUpWindow: 168h}}"))); err != nil || c.AnyFlux() {
		t.Fatalf("monitor without flux: %v %v", c.AnyFlux(), err)
	}
}

const depsPatch = `deps-patch: {author: {adopt: {logins: ["renovate[bot]"], branches: ['^renovate/']}}, diffShape: semver-patch,
    allowedFiles: ['^(apps|flux|infrastructure|observability|security|tooling)/.+\.ya?ml$'], protectedFiles: ['^security/base/zitadel/'],
    modifyOnly: true, maxPatchFiles: 3, maxModifiedLines: 10, releaseAgeCheck: renovate/stability-days, maxPerDay: 2}`

// MC10, MC11: an adopted class parses; author absent is the agents mode.
func TestAdoptedClassParses(t *testing.T) {
	c, err := Parse([]byte(withClasses(t, "docs-links: {}\n  revert: {}\n  "+depsPatch)))
	if err != nil {
		t.Fatal(err)
	}
	cl := c.RepositoryFor("Smana/cloud-native-ref").Classes["deps-patch"]
	if !cl.Adopted() || cl.Author.Adopt.Logins[0] != "renovate[bot]" || cl.ReleaseAgeCheck != "renovate/stability-days" || cl.DiffShape != "semver-patch" {
		t.Fatalf("%+v", cl)
	}
	if (Class{}).Adopted() {
		t.Fatal("no author is the agents mode")
	}
}

func TestBadAdoptionFails(t *testing.T) {
	base := "docs-links: {}\n  revert: {}\n  "
	for name, c := range map[string]struct{ classes, err string }{
		"semver without adopt":      {base + "x: {diffShape: semver-patch, modifyOnly: true}", "class x: diffShape semver-patch needs author.adopt (MC10)"},
		"adopt without semver":      {base + "x: {author: {adopt: {logins: [a], branches: ['^r/']}}}", "class x: author.adopt needs diffShape semver-patch or semver-minor"},
		"no logins":                 {base + "x: {author: {adopt: {logins: [], branches: ['^r/']}}, diffShape: semver-patch, modifyOnly: true}", "class x: author.adopt needs logins and branches"},
		"an unanchored branch":      {base + "x: {author: {adopt: {logins: [a], branches: ['renovate/']}}, diffShape: semver-patch, modifyOnly: true}", `class x: author.adopt.branches "renovate/" must start with ^`},
		"semver without modifyOnly": {base + "x: {author: {adopt: {logins: [a], branches: ['^r/']}}, diffShape: semver-patch}", "class x: a semver shape needs modifyOnly: true"},
		"a minor class live":        {base + "x: {live: true, author: {adopt: {logins: [a], branches: ['^r/']}}, diffShape: semver-minor, modifyOnly: true}", "class x: semver-minor is shadow only (M2)"},
		"release age, agents":       {base + "x: {releaseAgeCheck: renovate/stability-days}", "class x: releaseAgeCheck needs author.adopt"},
		"adopt with an oracle":      {base + "x: {author: {adopt: {logins: [a], branches: ['^r/']}}, diffShape: semver-patch, modifyOnly: true, holdoutChecks: [h], oracle: {probe: dead-links, workflow: d.yaml, maxFindings: 1}}", "class x: an oracle reads link targets: diffShape must be links-only"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(withClasses(t, c.classes))); err == nil || !strings.Contains(err.Error(), c.err) {
				t.Fatalf("got %v, want %q", err, c.err)
			}
		})
	}
}
