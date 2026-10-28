// SPDX-License-Identifier: Apache-2.0

// Package filefence is a class's file conditions on a diff (allowedFiles, protectedFiles, modifyOnly
// and the size limits): one implementation, used by the merge gate to decide a pull request and
// by the adopt poller to bind it to a class, so the two never disagree on what the fence is.
package filefence

import (
	"regexp"
	"slices"

	"github.com/Smana/maku/internal/factory/config"
	"github.com/Smana/maku/internal/factory/forge"
)

// Compile builds patterns once per decision. A pattern that does not compile is dropped, so an
// allow list fails closed and a protect list is caught by Validate before it runs. anchored wraps
// each as ^(?:p)$, so an alternation cannot leave one branch unanchored.
func Compile(pats []string, anchored bool) []*regexp.Regexp {
	var out []*regexp.Regexp
	for _, p := range pats {
		if anchored {
			p = "^(?:" + p + ")$"
		}
		if re, err := regexp.Compile(p); err == nil {
			out = append(out, re)
		}
	}
	return out
}

// MatchesAny reports whether any pattern matches path.
func MatchesAny(res []*regexp.Regexp, path string) bool {
	return slices.ContainsFunc(res, func(re *regexp.Regexp) bool { return re.MatchString(path) })
}

// Paths are every path the diff touches, a rename's two names included.
func Paths(stats []forge.FileStat) []string {
	var out []string
	for _, f := range stats {
		out = append(out, f.Path)
		if f.Previous != "" {
			out = append(out, f.Previous)
		}
	}
	return out
}

// AllowedFiles matches whole paths: policy-bot's match is unanchored, so the pattern is anchored
// here. "" when every path is allowed.
func AllowedFiles(cl config.Class, stats []forge.FileStat) string {
	res := Compile(cl.AllowedFiles, true)
	for _, p := range Paths(stats) {
		if !MatchesAny(res, p) {
			return "file_not_allowed"
		}
	}
	return ""
}

// ProtectedFiles refuses a diff touching a protected path.
func ProtectedFiles(cl config.Class, stats []forge.FileStat) string {
	res := Compile(cl.ProtectedFiles, false)
	if slices.ContainsFunc(Paths(stats), func(p string) bool { return MatchesAny(res, p) }) {
		return "file_protected"
	}
	return ""
}

// ModifyOnly refuses an added, removed or renamed file.
func ModifyOnly(stats []forge.FileStat) string {
	if slices.ContainsFunc(stats, func(f forge.FileStat) bool { return f.Status != "modified" }) {
		return "file_added_or_deleted"
	}
	return ""
}

// MaxPatchFiles refuses a diff of more files than the class's cap.
func MaxPatchFiles(cl config.Class, stats []forge.FileStat) string {
	if len(stats) > cl.MaxPatchFiles {
		return "patch_too_large"
	}
	return ""
}

// MaxModifiedLines refuses a diff of more changed lines than the class's cap.
func MaxModifiedLines(cl config.Class, stats []forge.FileStat) string {
	n := 0
	for _, f := range stats {
		n += f.Additions + f.Deletions
	}
	if n > cl.MaxModifiedLines {
		return "patch_too_large"
	}
	return ""
}

// Refusal is the first of the class's declared file conditions the diff breaks, in the gate's
// order, "" when it keeps them all.
func Refusal(cl config.Class, stats []forge.FileStat) string {
	switch {
	case len(cl.AllowedFiles) > 0 && AllowedFiles(cl, stats) != "":
		return AllowedFiles(cl, stats)
	case len(cl.ProtectedFiles) > 0 && ProtectedFiles(cl, stats) != "":
		return ProtectedFiles(cl, stats)
	case cl.ModifyOnly && ModifyOnly(stats) != "":
		return ModifyOnly(stats)
	case cl.MaxPatchFiles > 0 && MaxPatchFiles(cl, stats) != "":
		return MaxPatchFiles(cl, stats)
	case cl.MaxModifiedLines > 0 && MaxModifiedLines(cl, stats) != "":
		return MaxModifiedLines(cl, stats)
	}
	return ""
}
