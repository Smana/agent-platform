// SPDX-License-Identifier: Apache-2.0

package github

import (
	"strings"
	"unicode"
)

// Neutralise makes untrusted text, an agent's verdict summary, inert in a pull
// request comment while keeping it readable:
//   - format and control characters (bidi overrides, zero-width, BOM, soft
//     hyphen, escapes) are dropped, but newline and tab;
//   - '&' and '<' are escaped, so no HTML renders: no comment (a forged
//     marker), no <img>, no <details>, and no entity decodes back into one;
//   - "![" is escaped, so no markdown image loads a remote URL;
//   - '@' before a name becomes the fullwidth '＠', so nobody is notified.
func Neutralise(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	rs := []rune(s)
	for i, r := range rs {
		next := rune(0)
		if i+1 < len(rs) {
			next = rs[i+1]
		}
		switch {
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case unicode.Is(unicode.Cf, r) || unicode.IsControl(r):
		case r == '&':
			b.WriteString("&amp;")
		case r == '<':
			b.WriteString("&lt;")
		case r == '!' && next == '[':
			b.WriteString(`!\`)
		case r == '@' && (unicode.IsLetter(next) || unicode.IsDigit(next)):
			b.WriteRune('＠')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
