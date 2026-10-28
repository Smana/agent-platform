// SPDX-License-Identifier: Apache-2.0

package config

import (
	"slices"
	"strings"
	"testing"
)

// withTestFirst is withChecks plus a scripts class that writes its tests first and the repository's
// contract check.
func withTestFirst(t *testing.T) string {
	t.Helper()
	raw := strings.Replace(withChecks(t, "[pypi]"), "dashboards: {}", "dashboards: {}, scripts: {checks: true, testFirst: true}", 1)
	return strings.Replace(raw, "    merge:\n", "    merge:\n      contractCheck: \"Test contract 🧪\"\n", 1)
}

func TestTestFirstIsPerClassAndNeedsChecksAndTheContractCheck(t *testing.T) {
	c, err := Parse([]byte(withTestFirst(t)))
	if err != nil {
		t.Fatal(err)
	}
	r := c.RepositoryFor("Smana/cloud-native-ref")
	if !r.TestFirstFor("scripts") || r.TestFirstFor("docs") || r.Merge.ContractCheck != "Test contract 🧪" {
		t.Fatalf("scripts %v, docs %v, contractCheck %q", r.TestFirstFor("scripts"), r.TestFirstFor("docs"), r.Merge.ContractCheck)
	}
	for _, tc := range []struct{ name, from, to, want string }{
		{"no checks", "scripts: {checks: true, testFirst: true}", "scripts: {testFirst: true}", "testFirst needs checks: true"},
		{"no contract check", "      contractCheck: \"Test contract 🧪\"\n", "", "testFirst needs merge.contractCheck"},
		{"docs-links", "docs-links: {shadow: true}", "docs-links: {shadow: true, checks: true, testFirst: true}", "docs-links: testFirst is refused"},
		{"required", "      contractCheck: \"Test contract 🧪\"\n", "      contractCheck: \"Security scanning 🔒\"\n", "must not be one of merge.requiredChecks"},
	} {
		raw := strings.Replace(withTestFirst(t), tc.from, tc.to, 1)
		if raw == withTestFirst(t) {
			t.Fatalf("%s: the edit did not apply", tc.name)
		}
		if _, err := Parse([]byte(raw)); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}
}

func TestReviewMayBeDeclaredForChecksAndTestFirstOnly(t *testing.T) {
	ok := strings.Replace(withTestFirst(t), "scripts: {checks: true, testFirst: true}", "scripts: {}, review: {checks: true, testFirst: true}", 1)
	if _, err := Parse([]byte(ok)); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse([]byte(strings.Replace(ok, "review: {checks: true, testFirst: true}", "review: {live: true}", 1))); err == nil ||
		!strings.Contains(err.Error(), "review takes checks and testFirst only") {
		t.Fatalf("a live review parsed: %v", err)
	}
}

func TestATemplateListingTheTesterLoadsWithoutItAndWarns(t *testing.T) {
	c, err := Parse([]byte(listShaped(t, good)))
	if err != nil {
		t.Fatal(err)
	}
	if roles := c.Templates["trio"].Roles; slices.Contains(roles, "tester") {
		t.Fatalf("trio %v still lists the tester", roles)
	}
	if !slices.ContainsFunc(c.Warnings, func(w string) bool { return strings.Contains(w, "template trio: tester is the test writer") }) {
		t.Fatalf("warnings %v", c.Warnings)
	}
}

func TestALiveClassWithoutHoldoutsWarns(t *testing.T) {
	c, err := Parse([]byte(strings.Replace(listShaped(t, good), "docs-links: {shadow: true}", "docs-links: {live: true}", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(c.Warnings, func(w string) bool {
		return strings.Contains(w, "class docs-links of Smana/cloud-native-ref is live without holdoutChecks")
	}) {
		t.Fatalf("warnings %v", c.Warnings)
	}
}
