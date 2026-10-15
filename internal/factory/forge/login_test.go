// SPDX-License-Identifier: Apache-2.0

package forge

import "testing"

func TestSameLoginIgnoresCase(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"Merger-Bot[bot]", "merger-bot[bot]", true}, {"smana", "Smana", true}, {"smana", "smana2", false}, {"", "", true},
	} {
		if got := SameLogin(tc.a, tc.b); got != tc.want {
			t.Errorf("SameLogin(%q, %q) = %v", tc.a, tc.b, got)
		}
	}
}
