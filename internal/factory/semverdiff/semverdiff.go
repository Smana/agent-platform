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
)

var (
	// The suffix takes everything a pre-release, build metadata or a fourth component could add,
	// so none of them can hide beside the three numbers: a suffix refuses the line.
	versionRE = regexp.MustCompile(`(v?)(\d+)\.(\d+)\.(\d+)((?:[-+.][0-9A-Za-z.+-]*)?)`)
	digestRE  = regexp.MustCompile(`sha256:[0-9a-f]{64}`)
)

type version struct {
	prefix              string
	major, minor, patch int
	suffix              string
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

// line: the two lines are equal once versions and digests are masked, hold as many of each, at
// least one version moved, every moved version moves as allowed, keeps its prefix and carries no
// suffix, and a digest changed only where a version moved.
func line(a, b string, moves func(a, b version) bool) (bool, string) {
	if mask(a) != mask(b) {
		return false, "text other than versions and digests changed"
	}
	va, vb := versions(a), versions(b)
	da, db := digestRE.FindAllString(a, -1), digestRE.FindAllString(b, -1)
	if len(va) != len(vb) || len(da) != len(db) {
		return false, "a version or digest added or removed"
	}
	moved := false
	for i := range va {
		if va[i] == vb[i] {
			if va[i].suffix != "" {
				return false, "a version with a suffix"
			}
			continue
		}
		if va[i].prefix != vb[i].prefix || va[i].suffix != "" || vb[i].suffix != "" || !moves(va[i], vb[i]) {
			return false, "a version moved outside the shape"
		}
		moved = true
	}
	if !moved {
		return false, "no version moved (a digest alone is a digest update)"
	}
	return true, ""
}

func mask(s string) string {
	return versionRE.ReplaceAllString(digestRE.ReplaceAllString(s, "\x00D"), "\x00V")
}

// versions parses every version on the line. A number that overflows is no version: it gets a
// value unequal to every other, so the line refuses.
func versions(s string) []version {
	var out []version
	for i, m := range versionRE.FindAllStringSubmatch(s, -1) {
		v := version{prefix: m[1], suffix: m[5]}
		var errs [3]error
		v.major, errs[0] = strconv.Atoi(m[2])
		v.minor, errs[1] = strconv.Atoi(m[3])
		v.patch, errs[2] = strconv.Atoi(m[4])
		if errs[0] != nil || errs[1] != nil || errs[2] != nil {
			v = version{major: -1, minor: -1, patch: -1 - i, suffix: "overflow"}
		}
		out = append(out, v)
	}
	return out
}

func lines(s string) []string {
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}
