// SPDX-License-Identifier: Apache-2.0

// Package semverdiff decides whether a diff is a dependency bump of one kind, from the diff alone
// (merge gate classes MC11, R41): a label is a claim, the diff is the change.
package semverdiff

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode"
)

// A version is any three-number token (R41): the caller's allowedFiles and protectedFiles bound
// where one counts, not this package. A new digest is trusted to belong to the new tag, so a digest
// moves only attached to a moved version in one token, `<version>@sha256:<hex>` (Renovate's pin
// form). Everything between tokens must be byte-identical, and a file with a control character
// (tab aside), a changed trailing newline or no change at all is refused.
var versionRE = regexp.MustCompile(`(v?)(\d+)\.(\d+)\.(\d+)((?:[-+.][0-9A-Za-z.+-]*)?)(@sha256:[0-9a-f]{64})?`)

// The suffix takes everything a pre-release, build metadata or a fourth component could add, so
// none of them can hide beside the three numbers: a suffix refuses the line.
type version struct {
	prefix              string
	major, minor, patch int
	suffix, digest      string
}

// Patch reports whether every changed line moves versions within their major.minor to a higher patch.
func Patch(base, head map[string]string) (bool, string) {
	return check(base, head, func(a, b version) bool { return a.major == b.major && a.minor == b.minor && b.patch > a.patch })
}

// Minor reports whether every changed line moves versions within their major, to a higher minor or patch.
func Minor(base, head map[string]string) (bool, string) {
	return check(base, head, func(a, b version) bool {
		return a.major == b.major && (b.minor > a.minor || b.minor == a.minor && b.patch > a.patch)
	})
}

func check(base, head map[string]string, moves func(a, b version) bool) (bool, string) {
	files := slices.Sorted(maps.Keys(head))
	if len(files) == 0 || !slices.Equal(files, slices.Sorted(maps.Keys(base))) {
		return false, "no file, or a file added or deleted"
	}
	changed := 0
	for _, f := range files {
		if base[f] == "" || head[f] == "" {
			return false, f + ": added or deleted"
		}
		if base[f] == head[f] {
			return false, f + ": unchanged"
		}
		if hasControl(base[f]) || hasControl(head[f]) {
			return false, f + ": a control character"
		}
		if strings.HasSuffix(base[f], "\n") != strings.HasSuffix(head[f], "\n") {
			return false, f + ": trailing newline changed"
		}
		bl, hl := lines(base[f]), lines(head[f])
		if len(bl) != len(hl) {
			return false, f + ": lines added or removed"
		}
		for i := range bl {
			if bl[i] == hl[i] {
				continue
			}
			if ok, why := line(bl[i], hl[i], moves); !ok {
				return false, fmt.Sprintf("%s:%d: %s", f, i+1, why)
			}
			changed++
		}
	}
	if changed == 0 {
		return false, "nothing changed"
	}
	return true, ""
}

// line: the text between version tokens is byte-identical, both lines hold as many tokens, at
// least one version moved, every moved version moves as allowed, keeps its prefix, carries no
// suffix and no leading zero, and a digest differs only on a token whose version moved and keeps
// being attached.
func line(a, b string, moves func(a, b version) bool) (bool, string) {
	ia, ib := versionRE.FindAllStringSubmatchIndex(a, -1), versionRE.FindAllStringSubmatchIndex(b, -1)
	if len(ia) != len(ib) {
		return false, "a version added or removed"
	}
	va, vb := versions(a, ia), versions(b, ib)
	moved := false
	for i := range ia {
		if segment(a, ia, i) != segment(b, ib, i) {
			return false, "text other than versions changed"
		}
		x, y := va[i], vb[i]
		if x.suffix != "" || y.suffix != "" || x.major < 0 || y.major < 0 {
			return false, "a version with a suffix, a leading zero or an overflow"
		}
		if x == y {
			continue
		}
		if x.prefix != y.prefix || (x.digest == "") != (y.digest == "") || !moves(x, y) {
			return false, "a version moved outside the shape"
		}
		moved = true
	}
	if segment(a, ia, len(ia)) != segment(b, ib, len(ib)) {
		return false, "text other than versions changed"
	}
	if !moved {
		return false, "no version moved"
	}
	return true, ""
}

// segment is the text before token i (the tail after the last one when i == len(idx)).
func segment(s string, idx [][]int, i int) string {
	from, to := 0, len(s)
	if i > 0 {
		from = idx[i-1][1]
	}
	if i < len(idx) {
		to = idx[i][0]
	}
	return s[from:to]
}

// versions parses the tokens. One that overflows or has a leading zero gets a negative major,
// which the line refuses.
func versions(s string, idx [][]int) []version {
	out := make([]version, 0, len(idx))
	for _, m := range idx {
		g := func(n int) string {
			if m[2*n] < 0 {
				return ""
			}
			return s[m[2*n]:m[2*n+1]]
		}
		v := version{prefix: g(1), suffix: g(5), digest: g(6)}
		bad := false
		for k, dst := range []*int{&v.major, &v.minor, &v.patch} {
			d := g(k + 2)
			n, err := strconv.Atoi(d)
			if err != nil || len(d) > 1 && d[0] == '0' {
				bad = true
			}
			*dst = n
		}
		if bad {
			v.major = -1
		}
		out = append(out, v)
	}
	return out
}

// hasControl reports a control character (C0, DEL or C1) other than tab and the newline that splits
// lines.
func hasControl(s string) bool {
	return strings.ContainsFunc(s, func(r rune) bool { return unicode.IsControl(r) && r != '\t' && r != '\n' })
}

func lines(s string) []string {
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}
