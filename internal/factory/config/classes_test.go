// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
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
	if !cl.ReadsDiff("dead-links") || !(Class{}).ReadsDiff("docs-links") || (Class{Shadow: true}).ReadsDiff("docs") {
		t.Fatal("ReadsDiff: a declared file condition or a built-in shape reads the diff, nothing else does")
	}
}

// Offline proof 3: an unknown condition fails the strict decoder; a bad value fails Validate.
func TestBadConditionsFail(t *testing.T) {
	for name, c := range map[string]struct{ classes, err string }{
		"gh-aw's spelling":   {"docs-links: {live: true, max-patch-size: 3}\n  revert: {}", `unknown field "max-patch-size"`},
		"an unknown key":     {"docs-links: {live: true, maxPatchSize: 3}\n  revert: {}", `unknown field "maxPatchSize"`},
		"a bad pattern":      {"docs-links: {}\n  revert: {}\n  x: {allowedFiles: ['(']}", `class x: allowedFiles "(" is not a regular expression`},
		"an empty pattern":   {"docs-links: {}\n  revert: {}\n  x: {protectedFiles: ['']}", `class x: protectedFiles "" is not a regular expression`},
		"a negative cap":     {"docs-links: {}\n  revert: {}\n  x: {maxModifiedLines: -1}", "class x: maxModifiedLines must be 0 (no cap) or more"},
		"an unknown shape":   {"docs-links: {}\n  revert: {}\n  x: {diffShape: semver-patch}", `class x: diffShape "semver-patch" is not one of [links-only]`},
		"a holdout twice":    {"docs-links: {}\n  revert: {}\n  x: {holdoutChecks: [a, a]}", `class x: holdoutChecks names "a" empty or twice`},
		"live and shadow":    {"docs-links: {live: true, shadow: true}\n  revert: {}", "class docs-links: live and shadow never together (R32)"},
		"a negative per day": {"docs-links: {}\n  revert: {}\n  x: {maxPerDay: -2}", "class x: maxPerDay must be 0 (no cap) or more"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(withClasses(t, c.classes)))
			if err == nil || !strings.Contains(err.Error(), c.err) {
				t.Fatalf("got %v, want %q", err, c.err)
			}
		})
	}
}
