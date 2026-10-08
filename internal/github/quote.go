// SPDX-License-Identifier: Apache-2.0

package github

import (
	"strings"
	"unicode"
)

// Quote makes untrusted text, an agent's verdict summary, inert in a pull
// request comment: a fenced code block that nothing in the text can close,
// because its fence is longer than any run of backticks the text holds. GitHub
// renders a code block as text only: no mention notifies anyone, no
// cross-reference (a URL, #N, GH-N, owner/repo#N, a SHA) backlinks another
// issue, no image loads and no HTML renders, a marker included (reviews 3.4 I1,
// 3.5 m3). Hidden characters go first: format and control characters but
// newline and tab, every other default-ignorable code point and the variation
// selectors (bidi overrides, zero-width joiners, the BOM, soft hyphens, escapes).
func Quote(s string) string {
	text := strings.Map(func(r rune) rune {
		if r != '\n' && r != '\t' && (unicode.Is(unicode.Cf, r) || unicode.IsControl(r) ||
			unicode.Is(unicode.Other_Default_Ignorable_Code_Point, r) || unicode.Is(unicode.Variation_Selector, r)) {
			return -1
		}
		return r
	}, s)
	longest, run := 0, 0
	for _, r := range text {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	fence := strings.Repeat("`", max(3, longest+1))
	return fence + "\n" + text + "\n" + fence
}
