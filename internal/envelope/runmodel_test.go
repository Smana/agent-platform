// SPDX-License-Identifier: Apache-2.0

package envelope

import (
	"strings"
	"testing"
)

func TestValidModel(t *testing.T) {
	for name, tc := range map[string]struct {
		s  string
		ok bool
	}{
		"a C5 logical name":              {"agent-default", true},
		"a provider path with a version": {"anthropic/claude-opus-4.5@2026", true},
		"exactly 64 bytes":               {strings.Repeat("m", 64), true},
		"empty":                          {"", false},
		"a space":                        {"agent default", false},
		"markup":                         {"<b>x</b>", false},
		"over 64 bytes":                  {strings.Repeat("m", 65), false},
		"starting with punctuation":      {"-rf", false},
		"a newline":                      {"a\nb", false},
		"a bidi override":                {"x\u202ey", false},
		"a redaction marker":             {"[REDACTED:github-app-token]", false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := ValidModel(tc.s); got != tc.ok {
				t.Fatalf("ValidModel(%q) = %v, want %v", tc.s, got, tc.ok)
			}
		})
	}
}

func TestValidEffort(t *testing.T) {
	for _, e := range []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"} {
		if !ValidEffort(e) {
			t.Errorf("ValidEffort(%q) = false", e)
		}
	}
	for _, e := range []string{"", "HIGH", "turbo", "high ", "[REDACTED:x]"} {
		if ValidEffort(e) {
			t.Errorf("ValidEffort(%q) = true", e)
		}
	}
	if len(Efforts) != 7 {
		t.Fatalf("the effort set is a bounded label: %d values", len(Efforts))
	}
}
