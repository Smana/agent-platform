// SPDX-License-Identifier: Apache-2.0

package config

import (
	"regexp"
	"slices"
)

// Class is one merge class (§5.2): live classes auto-merge once policy-bot agrees; a shadow
// class is decided exactly like a live one and never armed until the wave (R32, owner,
// 2026-09-27); an unmarked class is a prediction and a PR label only (OD-8). Never both.
//
// The other fields are the class's declared conditions (merge gate classes MC1), named as gh-aw
// names them where it has a name. Each is checked on top of the gate's fixed base, which no class
// can remove; an absent one is not checked, so a class that declares none is decided as before.
type Class struct {
	Live   bool `json:"live,omitempty"`
	Shadow bool `json:"shadow,omitempty"`
	// AllowedFiles: every changed path matches one of these regular expressions.
	AllowedFiles []string `json:"allowedFiles,omitempty"`
	// ProtectedFiles: no changed path matches one of these.
	ProtectedFiles []string `json:"protectedFiles,omitempty"`
	// ModifyOnly: no file is added, deleted, renamed or copied.
	ModifyOnly bool `json:"modifyOnly,omitempty"`
	// MaxPatchFiles caps the changed files and MaxModifiedLines their additions plus deletions,
	// as GitHub counts them (policy-bot's modified_lines); 0 is no cap.
	MaxPatchFiles    int `json:"maxPatchFiles,omitempty"`
	MaxModifiedLines int `json:"maxModifiedLines,omitempty"`
	// DiffShape is a shape every changed line must have: links-only is R52's LinksOnly.
	DiffShape string `json:"diffShape,omitempty"`
	// HoldoutChecks are checks GitHub Actions must report SUCCESS on the head; their workflow is
	// a gate path, so the pull request cannot change them (loops L7).
	HoldoutChecks []string `json:"holdoutChecks,omitempty"`
	// MaxPerDay caps this class's merges a day, under merge.autoMergesPerDay; 0 is no cap of its own.
	MaxPerDay int `json:"maxPerDay,omitempty"`
}

// DiffShapes are the shapes diffShape names.
func DiffShapes() []string { return []string{"links-only"} }

// BuiltInShape is the diff shape the gate holds a class to when its config names none: R52's
// promise of the two entry classes (MC1: their conditions become built-in defaults at parity).
func BuiltInShape(name string) string {
	if name == "docs-links" || name == "revert" {
		return "links-only"
	}
	return ""
}

// ReadsDiff reports whether deciding the class reads the pull request's diff: a built-in shape
// or any declared condition on its files.
func (c Class) ReadsDiff(name string) bool {
	return BuiltInShape(name) != "" || len(c.AllowedFiles) > 0 || len(c.ProtectedFiles) > 0 || c.ModifyOnly ||
		c.MaxPatchFiles > 0 || c.MaxModifiedLines > 0 || c.DiffShape != ""
}

// validateClass reports one class's problems.
func validateClass(name string, cl Class, bad func(string, ...any)) {
	if cl.Live && cl.Shadow {
		bad("class %s: live and shadow never together (R32)", name)
	}
	for _, f := range []struct {
		key  string
		pats []string
	}{{"allowedFiles", cl.AllowedFiles}, {"protectedFiles", cl.ProtectedFiles}} {
		for _, p := range f.pats {
			if _, err := regexp.Compile(p); err != nil || p == "" {
				bad("class %s: %s %q is not a regular expression", name, f.key, p)
			}
		}
	}
	for _, f := range []struct {
		key string
		n   int
	}{{"maxPatchFiles", cl.MaxPatchFiles}, {"maxModifiedLines", cl.MaxModifiedLines}, {"maxPerDay", cl.MaxPerDay}} {
		if f.n < 0 {
			bad("class %s: %s must be 0 (no cap) or more", name, f.key)
		}
	}
	if cl.DiffShape != "" && !slices.Contains(DiffShapes(), cl.DiffShape) {
		bad("class %s: diffShape %q is not one of %v", name, cl.DiffShape, DiffShapes())
	}
	if b := BuiltInShape(name); b != "" && cl.DiffShape != "" && cl.DiffShape != b {
		bad("class %s: diffShape is %s, built in (R52)", name, b)
	}
	seen := map[string]bool{}
	for _, h := range cl.HoldoutChecks {
		if h == "" || seen[h] {
			bad("class %s: holdoutChecks names %q empty or twice", name, h)
		}
		seen[h] = true
	}
}
