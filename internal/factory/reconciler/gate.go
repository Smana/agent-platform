// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"regexp"
	"slices"

	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/forge"
)

// Condition is one condition a class declares in its config (merge gate classes MC1): Declared
// reads whether the class names it; Check is "" when it holds on the decision's inputs, else the
// reason it refuses with. A condition on the diff holds until the diff is read (in.DiffRead):
// awaitingCI reads it only for a decision that would arm or shadow, then decides again.
type Condition struct {
	Key      string
	Declared func(cl config.Class) bool
	Check    func(cl config.Class, in ArmInputs) (reason string, wait bool)
}

// Conditions are the declared conditions, in the order the gate checks them.
func Conditions() []Condition {
	return []Condition{
		{"allowedFiles", func(cl config.Class) bool { return len(cl.AllowedFiles) > 0 }, allowedFiles},
		{"protectedFiles", func(cl config.Class) bool { return len(cl.ProtectedFiles) > 0 }, protectedFiles},
		{"modifyOnly", func(cl config.Class) bool { return cl.ModifyOnly }, modifyOnly},
		{"maxPatchFiles", func(cl config.Class) bool { return cl.MaxPatchFiles > 0 }, maxPatchFiles},
		{"maxModifiedLines", func(cl config.Class) bool { return cl.MaxModifiedLines > 0 }, maxModifiedLines},
		{"diffShape", func(cl config.Class) bool { return cl.DiffShape != "" }, diffShape},
		{"holdoutChecks", func(cl config.Class) bool { return len(cl.HoldoutChecks) > 0 }, holdoutChecks},
	}
}

// changedPaths are every path the diff touches, a rename's two names included.
func changedPaths(in ArmInputs) []string {
	var out []string
	for _, f := range in.Stats {
		out = append(out, f.Path)
		if f.Previous != "" {
			out = append(out, f.Previous)
		}
	}
	return out
}

// compile builds a class's patterns once per decision. A pattern that does not compile is
// dropped, so an allow list fails closed and a protect list is caught by Validate before it runs.
// anchored wraps each as ^(?:p)$, so an alternation cannot leave one branch unanchored.
func compile(pats []string, anchored bool) []*regexp.Regexp {
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

func matchesAny(res []*regexp.Regexp, path string) bool {
	return slices.ContainsFunc(res, func(re *regexp.Regexp) bool { return re.MatchString(path) })
}

// allowedFiles matches whole paths: policy-bot's match is unanchored, so the gate anchors the
// whole pattern itself.
func allowedFiles(cl config.Class, in ArmInputs) (string, bool) {
	res := compile(cl.AllowedFiles, true)
	for _, p := range changedPaths(in) {
		if !matchesAny(res, p) {
			return "file_not_allowed", false
		}
	}
	return "", false
}

func protectedFiles(cl config.Class, in ArmInputs) (string, bool) {
	res := compile(cl.ProtectedFiles, false)
	if slices.ContainsFunc(changedPaths(in), func(p string) bool { return matchesAny(res, p) }) {
		return "file_protected", false
	}
	return "", false
}

func modifyOnly(_ config.Class, in ArmInputs) (string, bool) {
	if slices.ContainsFunc(in.Stats, func(f forge.FileStat) bool { return f.Status != "modified" }) {
		return "file_added_or_deleted", false
	}
	return "", false
}

func maxPatchFiles(cl config.Class, in ArmInputs) (string, bool) {
	if len(in.Stats) > cl.MaxPatchFiles {
		return "patch_too_large", false
	}
	return "", false
}

func maxModifiedLines(cl config.Class, in ArmInputs) (string, bool) {
	n := 0
	for _, f := range in.Stats {
		n += f.Additions + f.Deletions
	}
	if n > cl.MaxModifiedLines {
		return "patch_too_large", false
	}
	return "", false
}

// diffShape: links-only is R52's promise, read on in.Files as it always was, so a diff not read
// yet passes it vacuously (LinksOnly of two empty trees).
func diffShape(cl config.Class, in ArmInputs) (string, bool) {
	if cl.DiffShape == "links-only" {
		if ok, _ := LinksOnly(in.Files.Base, in.Files.Head); !ok {
			return "class_mismatch", false
		}
	}
	return "", false
}

// holdoutChecks (loops L7): each named check passed on the head, posted by GitHub Actions, and
// every run of that name did: the raw conclusion must be SUCCESS, since a skipped or neutral job
// (an if: or paths filter) reads State SUCCESS and checked nothing. Not reported yet, or still
// running, waits; any run failed, cancelled, skipped or posted by another App refuses, and so does
// a rollup read in part (Checks.Truncated): a run of the name may be unseen.
func holdoutChecks(cl config.Class, in ArmInputs) (string, bool) {
	if in.Checks.Truncated { // fail closed: a run of the name may sit past the page read
		return "holdout_red", false
	}
	wait := false
	for _, name := range cl.HoldoutChecks {
		seen := false
		for _, run := range in.Checks.Runs {
			if run.Name != name {
				continue
			}
			seen = true
			switch {
			case run.State == "PENDING":
				wait = true
			case run.App != forge.ActionsApp || run.State != "SUCCESS" || run.Conclusion != "SUCCESS":
				return "holdout_red", false
			}
		}
		wait = wait || !seen
	}
	if wait {
		return "holdout_pending", true
	}
	return "", false
}

// onDiff are the conditions that read the diff, which hold until it is read.
var onDiff = []string{"allowedFiles", "protectedFiles", "modifyOnly", "maxPatchFiles", "maxModifiedLines"}

// declared checks the class's declared conditions in order: the first refusal, or "".
func declared(cl config.Class, in ArmInputs) (string, bool) {
	for _, c := range Conditions() {
		if !c.Declared(cl) || (!in.DiffRead && slices.Contains(onDiff, c.Key)) {
			continue
		}
		if reason, wait := c.Check(cl, in); reason != "" {
			return reason, wait
		}
	}
	return "", false
}
