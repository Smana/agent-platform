// SPDX-License-Identifier: Apache-2.0

// Package mdlinks reads Markdown link targets the way the merge gate's links-only shape does
// (R52): a link target is an inline one's or a reference definition's, outside a code fence. The
// gate, the dead-links probe and the oracle read targets through it alone, so the three agree.
package mdlinks

import (
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// reLinkTarget matches one link's target: a reference definition's ([label]: target at a line
// start) or an inline one's (](target), angle-bracketed or bare). Titles, labels and every other
// byte stay in the compared text, so a change beside a link is visible. Deliberately not a full
// markdown parser: a diff that hides a change inside what this reads as a link target is refused
// by the target rule below, and the classes it gates stay shadow until the wave re-reads this
// against a month of forecasts (task 7.9, R52).
var reLinkTarget = regexp.MustCompile(`(?m)^ {0,3}\[[^()\]\r\n]+\]:[ \t]*(<[^<>\r\n]*>|\S+)|\]\((<[^<>\r\n]*>|[^\s()<]*)`)

// linkPlaceholder blanks a target so the rest of the text compares byte for byte. NUL: not in
// any file a class like this edits, and counted below if one ever appears.
const linkPlaceholder = "\x00"

// Targets is the text with every link target outside a code fence replaced by the placeholder,
// and those targets in order of appearance. Fenced lines are left unsubstituted on purpose:
// fenced content is code, and a docs-links diff that moved a link inside a fence moved code,
// which the byte compare below then sees.
func Targets(s string) (string, []string) {
	var out []string
	m := reLinkTarget.FindAllStringSubmatchIndex(s, -1)
	if m == nil {
		return s, nil
	}
	fences := fencesOf(s)
	var b strings.Builder
	last := 0
	for _, ix := range m {
		if inFence(fences, ix[0]) {
			continue // the fenced text still gets written: last never jumps over it
		}
		start, end := ix[4], ix[5] // the inline alternative
		if ix[2] >= 0 {
			start, end = ix[2], ix[3] // the reference-definition alternative
		}
		b.WriteString(s[last:start])
		b.WriteString(linkPlaceholder)
		t := s[start:end]
		if len(t) >= 2 && strings.HasPrefix(t, "<") {
			t = t[1 : len(t)-1]
		}
		out = append(out, t)
		last = end
	}
	b.WriteString(s[last:])
	return b.String(), out
}

// fencesOf are the byte ranges of ```-fenced blocks, fence lines included. An unbalanced
// fence runs to the end of the text: its links go uncounted, which fails the byte compare
// for any change there — the safe direction.
func fencesOf(s string) [][2]int {
	var out [][2]int
	in, start := false, 0
	for off := 0; off < len(s); {
		end := strings.IndexByte(s[off:], '\n')
		if end < 0 {
			end = len(s)
		} else {
			end += off
		}
		if strings.HasPrefix(strings.TrimLeft(s[off:end], " \t"), "```") {
			if in {
				out, in = append(out, [2]int{start, end}), false
			} else {
				start, in = off, true
			}
		}
		if end == len(s) {
			break
		}
		off = end + 1
	}
	if in {
		out = append(out, [2]int{start, len(s)})
	}
	return out
}

func inFence(ranges [][2]int, off int) bool {
	return slices.ContainsFunc(ranges, func(r [2]int) bool { return r[0] <= off && off < r[1] })
}

// targetKind is a link target's scheme and host, with ("", "") only for a target relative to
// the current document. A schemeless one with a host is protocol-relative — host-absolute, and
// its non-empty host keeps it out of the relative clause. An unparseable one gets a scheme of
// its own so no pair can match it.
func targetKind(t string) (scheme, host string) {
	u, err := url.Parse(t)
	if err != nil {
		return "!", ""
	}
	if u.Scheme == "" && u.Host == "" {
		return "", "" // a path, a query, a fragment: relative
	}
	return strings.ToLower(u.Scheme), strings.ToLower(u.Host)
}

// retargetOK is R52's rule for a changed pair: relative to relative — hosts empty on both sides,
// so a protocol-relative //host is never "relative" — or https staying on its host, or landing
// on the archive. Never another scheme, never a host jump.
func retargetOK(from, to string) bool {
	fs, fh := targetKind(from)
	ts, th := targetKind(to)
	switch {
	case fs == "" && fh == "" && ts == "" && th == "":
		return true
	case fs == "https" && ts == "https" && (fh == th || th == "web.archive.org"):
		return true
	default:
		return false
	}
}

// LinksOnly is R52's promise of the docs-links and revert classes: between two commits of the
// tree, every changed file changed only its link targets, the counts are equal, and every
// retarget moves relatively, keeps its host, or lands on web.archive.org. The bool decides;
// the string is the first violation, named for the shadow forecast's record (task 7.9).
func LinksOnly(base, head map[string]string) (bool, string) {
	paths := make([]string, 0, len(base)+len(head))
	for p := range base {
		paths = append(paths, p)
	}
	for p := range head {
		if _, ok := base[p]; !ok {
			paths = append(paths, p)
		}
	}
	slices.Sort(paths)
	for _, p := range paths {
		b, h := base[p], head[p]
		if b == h {
			continue
		}
		nb, tb := Targets(b)
		nh, th := Targets(h)
		if nb != nh {
			return false, p + ": changed beyond link targets"
		}
		if len(tb) != len(th) { // redundant with nb == nh unless the text holds a real NUL
			return false, p + ": added or removed a link"
		}
		for i := range tb {
			if tb[i] != th[i] && !retargetOK(tb[i], th[i]) {
				return false, p + ": retargeted " + tb[i] + " to " + th[i]
			}
		}
	}
	return true, ""
}
