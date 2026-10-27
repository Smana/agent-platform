// SPDX-License-Identifier: Apache-2.0

package sanitize

import (
	"regexp"
	"strings"
)

var (
	// mdLink is an inline link on one line; its URL stops at the first space, ")" or backtick.
	mdLink = regexp.MustCompile(`\[([^\]\n]*)\]\(([^)\s` + "`" + `]*)[^)\n` + "`" + `]*\)`)
	// autolink is what GitHub turns into something live without markup, in one pass so a match is
	// never matched again: a scheme URL, a www. host, an email (GFM links); an owner/repo#n
	// reference; and a @mention or #n reference, which ping a person or write a backlink. A URL
	// stops at a backslash: the escaped backtick codeSpans leaves must not end up inside the span
	// built around it. A mention or a bare #n needs a non-word character before it, as on GitHub, so
	// "C#" and "fix#9" are left alone; that character is outside the match.
	autolink = regexp.MustCompile("(?m)" +
		"(?:(?:https?|ftp)://|www\\.)[^\\s<>`\\\\]+" +
		"|[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\\.[A-Za-z0-9-]+)+" +
		"|[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+#[0-9]+" +
		"|(?:^|[^A-Za-z0-9_`\\\\])(@[A-Za-z0-9][A-Za-z0-9-]*(?:/[A-Za-z0-9_.-]+)?|#[0-9]+)")
)

// Public makes text written by an agent safe to publish as a GitHub issue (RunLore agent-to-agent
// RA9, R8): Text's defusing, then every link inert. An inline link keeps its text and shows its URL
// in a code span; a bare URL, www. host or email becomes a code span. GFM never autolinks inside a
// code span, so no link node survives.
func Public(s string) (string, Report) {
	out, rep := Text(s)
	var b strings.Builder
	for _, seg := range codeSpans(out) {
		if seg.code {
			b.WriteString(seg.text)
			continue
		}
		t := mdLink.ReplaceAllString(seg.text, "$1 (`$2`)")
		// A destination the pattern did not take (a nested bracket, a title) is no link either:
		// "](" is what makes one.
		t = strings.ReplaceAll(t, "](", "] (")
		t = codeSpanLive(t)
		b.WriteString(t)
	}
	return b.String(), rep
}

type segment struct {
	text string
	code bool
}

// codeSpans splits s into code spans and the text between them, as CommonMark reads backticks: a
// run of n backticks opens a span the next run of exactly n closes; a backslash escapes the next
// character outside a span. A run that closes nothing is literal text, and escaped here, so the
// code spans Public adds can never pair with it.
func codeSpans(s string) []segment {
	var out []segment
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, segment{text: cur.String()})
			cur.Reset()
		}
	}
	for i := 0; i < len(s); {
		switch s[i] {
		case '\\':
			cur.WriteByte(s[i])
			if i+1 < len(s) {
				cur.WriteByte(s[i+1])
			}
			i += 2
		case '`':
			n := 0
			for i+n < len(s) && s[i+n] == '`' {
				n++
			}
			if end := closingRun(s, i+n, n); end >= 0 {
				flush()
				out = append(out, segment{text: s[i:end], code: true})
				i = end
				continue
			}
			cur.WriteString(strings.Repeat("\\`", n))
			i += n
		default:
			cur.WriteByte(s[i])
			i++
		}
	}
	flush()
	return out
}

// closingRun is the index just past the first run of exactly n backticks at or after from, or -1.
func closingRun(s string, from, n int) int {
	for i := from; i < len(s); {
		if s[i] != '`' {
			i++
			continue
		}
		j := i
		for j < len(s) && s[j] == '`' {
			j++
		}
		if j-i == n {
			return j
		}
		i = j
	}
	return -1
}

// codeSpanLive wraps every autolink, mention and reference of t in a code span.
func codeSpanLive(t string) string {
	var b strings.Builder
	last := 0
	for _, m := range autolink.FindAllStringSubmatchIndex(t, -1) {
		start, end := m[0], m[1]
		if m[2] >= 0 { // a mention or #n: the character before it stays outside
			start = m[2]
		}
		b.WriteString(t[last:start])
		b.WriteString("`" + t[start:end] + "`")
		last = end
	}
	b.WriteString(t[last:])
	return b.String()
}
