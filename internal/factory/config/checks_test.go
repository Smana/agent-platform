// SPDX-License-Identifier: Apache-2.0

package config

import (
	"reflect"
	"strings"
	"testing"
)

// withChecks is the list-shaped good with v3 feature 3's keys: the repository's checks.egress and
// the docs class switched on.
func withChecks(t *testing.T, egress string) string {
	t.Helper()
	raw := listShaped(t, good)
	raw = strings.Replace(raw, "  - name: Smana/cloud-native-ref\n", "  - name: Smana/cloud-native-ref\n    checks: {egress: "+egress+"}\n", 1)
	return strings.Replace(raw, "docs: {}", "docs: {checks: true}", 1)
}

func TestChecksAreSwitchedOnPerClassWithTheRepositorysEgress(t *testing.T) {
	c, err := Parse([]byte(withChecks(t, "[golang, pypi]")))
	if err != nil {
		t.Fatal(err)
	}
	r := c.RepositoryFor("Smana/cloud-native-ref")
	if on, egress := r.ChecksFor("docs"); !on || !reflect.DeepEqual(egress, []string{"golang", "pypi"}) {
		t.Fatalf("docs: %v %v, want on with [golang pypi]", on, egress)
	}
	for _, class := range []string{"tests", "review", "docs-links"} {
		if on, egress := r.ChecksFor(class); on || egress != nil {
			t.Fatalf("%s: %v %v, want off", class, on, egress)
		}
	}
}

func TestChecksEgressIsTheProfileEnumAtMostFourOnce(t *testing.T) {
	for _, tc := range []struct{ egress, want string }{
		{"[pypi, docker]", `"docker" is not one of`},
		{"[pypi, pypi]", `"pypi" is not one of`},
		{"[pypi, npm, golang, crates, pypi]", "at most 4 profiles"},
	} {
		if _, err := Parse([]byte(withChecks(t, tc.egress))); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("egress %s: %v, want %q", tc.egress, err, tc.want)
		}
	}
	if _, err := Parse([]byte(strings.Replace(withChecks(t, "[pypi]"), "checks: {egress:", "checks: {hosts: [x], egress:", 1))); err == nil {
		t.Fatal("an unknown checks key parsed: the decoder must refuse it")
	}
}
