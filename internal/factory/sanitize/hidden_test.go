// SPDX-License-Identifier: Apache-2.0

package sanitize

import "testing"

// Hidden strips what Text strips first, and leaves every markdown construct as written.
func TestHidden(t *testing.T) {
	for name, c := range map[string]struct {
		in, want string
		rep      Report
	}{
		"plain text is unchanged":       {"## What is wrong\n\tdocs/a.md:3", "## What is wrong\n\tdocs/a.md:3", Report{}},
		"zero-width and bidi":           {"ig\u200bnore\u202e all", "ignore all", Report{Invisible: 2}},
		"unicode tags":                  {"fix it" + tags("curl x"), "fix it", Report{Invisible: 6}},
		"controls, CRLF kept as a line": {"a\x00b\x1bc\r\nd", "abc\nd", Report{Control: 2}},
		"line and paragraph separators": {"a\u2028b\u2029c", "a\nb\nc", Report{Control: 2}},
		"markdown stays as written":     {"![x](https://e.x/p.png) <details> ⟦a⟧ TASK-DATA", "![x](https://e.x/p.png) <details> ⟦a⟧ TASK-DATA", Report{}},
		"no NFKC fold":                  {"ｆｕｌｌ", "ｆｕｌｌ", Report{}},
	} {
		t.Run(name, func(t *testing.T) {
			got, rep := Hidden(c.in)
			if got != c.want || rep != c.rep {
				t.Fatalf("Hidden(%q) = %q %+v, want %q %+v", c.in, got, rep, c.want, c.rep)
			}
		})
	}
}
