// SPDX-License-Identifier: Apache-2.0

package roomview

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Smana/agent-platform/internal/envelope"
)

// maxText bounds the text a view quotes, in runes.
const maxText = 160

// clean is untrusted text made safe to put on one line: whitespace collapsed to single spaces,
// hidden characters (control, zero-width, bidi) gone, and at most maxText runes.
func clean(s string) string {
	s = strings.Map(func(r rune) rune {
		if envelope.Control(r) {
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > maxText {
		return string(r[:maxText]) + "…"
	}
	return s
}

// code is s in a code span, the one place untrusted text can sit: the fence is longer than any run
// of backticks in s, and a space on each side keeps a leading or trailing backtick from joining it (a
// span with neither stays unpadded, as the design's examples read). In
// a table cell every pipe becomes U+2223 (a divides sign), never a backslash escape: parsers
// disagree on what a backslash before a pipe means (cmark-gfm, goldmark, pandoc), so no pipe may
// reach a cell at all.
func code(s string, cell bool) string {
	s = clean(s)
	longest, run := 0, 0
	for _, r := range s {
		if r == '`' {
			run++
			longest = max(longest, run)
		} else {
			run = 0
		}
	}
	if cell {
		s = strings.ReplaceAll(s, "|", cellPipe)
	}
	fence := strings.Repeat("`", longest+1)
	if s == "" || strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") {
		s = " " + s + " "
	}
	return fence + s + fence
}

// cellPipe stands for a pipe inside a table cell.
const cellPipe = "\u2223"

// punct is what escapeTitle escapes with a backslash: every character Markdown can read as syntax
// in the middle of a line, a pipe included for a table cell.
const punct = "\\`*_{}[]<>()#+-.!|~&"

// escapeTitle is s as plain Markdown text: its syntax escaped, and each word an autolink would
// catch (a URL, a www. host, an email address) in a code span instead.
func escapeTitle(s string, cell bool) string {
	words := strings.Fields(clean(s))
	for i, w := range words {
		if strings.Contains(w, "://") || strings.HasPrefix(strings.ToLower(w), "www.") || strings.Contains(w, "@") {
			words[i] = code(w, cell)
			continue
		}
		if cell {
			w = strings.ReplaceAll(w, "|", cellPipe)
		}
		var b strings.Builder
		for _, r := range w {
			if r < 0x80 && strings.ContainsRune(punct, r) {
				b.WriteByte('\\')
			}
			b.WriteRune(r)
		}
		words[i] = b.String()
	}
	return strings.Join(words, " ")
}

// quote is s for a terminal: cleaned, on one line, in double quotes.
func quote(s string) string {
	s = strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(clean(s))
	return `"` + s + `"`
}

// roomPath is what follows the rooms base in a room link: the room, then an optional fragment.
var roomPath = regexp.MustCompile(`^/r/[a-z0-9]{1,32}(#[A-Za-z0-9_-]{1,64})?$`)

// allowed reports whether url is one the broker builds: a room page on the rooms base, with an
// optional fragment, or an issue or pull request on GitHub.
func allowed(url string, o Options) bool {
	if envelope.GitHubURL(url) {
		return true
	}
	base := strings.TrimRight(o.Rooms, "/")
	if base == "" || !strings.HasPrefix(url, base) {
		return false
	}
	return roomPath.MatchString(strings.TrimPrefix(url, base))
}

// link is text linking to url in Markdown, text (url) otherwise, and the text alone when url is
// off the allowlist: an agent's text never makes a link, only the broker's own URLs do.
func link(text, url string, o Options, cell bool) string {
	if o.Format != Markdown {
		text = clean(text)
		if !allowed(url, o) {
			return text
		}
		return text + " (" + url + ")"
	}
	text = escapeTitle(text, cell)
	if !allowed(url, o) {
		return text
	}
	return "[" + text + "](" + url + ")"
}

// repoOf is the owner/name of the first URL that is an issue or pull request on GitHub, else "".
func repoOf(urls ...string) string {
	for _, u := range urls {
		if envelope.GitHubURL(u) {
			p := strings.Split(strings.TrimPrefix(u, "https://github.com/"), "/")
			return p[0] + "/" + p[1]
		}
	}
	return ""
}

// dollars is micro-dollars as dollars, half a cent rounding up: 310000 reads $0.31.
func dollars(micro int64) string {
	if micro < 0 {
		micro = 0
	}
	cents := (micro + 5000) / 10000
	return fmt.Sprintf("$%d.%02d", cents/100, cents%100)
}

// ago is how long before now t was: minutes, then hours, and from a day on its date.
func ago(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d >= 24*time.Hour:
		return t.UTC().Format("2006-01-02")
	case d >= time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + " h ago"
	}
	return strconv.Itoa(max(0, int(d/time.Minute))) + " min ago"
}

// stamp is t in UTC, and says so: the time alone on now's UTC day, else the date and the time.
func stamp(t, now time.Time) string {
	t, now = t.UTC(), now.UTC()
	if t.YearDay() == now.YearDay() && t.Year() == now.Year() {
		return t.Format("15:04 UTC")
	}
	return t.Format("2006-01-02 15:04 UTC")
}

// tokens reads a token count at a glance, as the page does: 189093 -> "189 k", 1500000 -> "1.5 M".
// A count that would round to "1000 k" reads "1 M".
func tokens(n int64) string {
	switch {
	case n < 1000:
		return strconv.FormatInt(n, 10)
	case n < 999_500:
		return strconv.FormatInt((n+500)/1000, 10) + " k"
	}
	return strconv.FormatFloat(float64((n+50_000)/100_000)/10, 'f', -1, 64) + " M"
}
