// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"cmp"
	"maps"
	"regexp"
	"slices"

	v1alpha1 "github.com/Smana/maku/api/factory/v1alpha1"

	"github.com/Smana/maku/internal/factory/config"
	"github.com/Smana/maku/internal/factory/decision"
	"github.com/Smana/maku/internal/factory/filefence"
	"github.com/Smana/maku/internal/factory/forge"
	"github.com/Smana/maku/internal/factory/mdlinks"
	"github.com/Smana/maku/internal/factory/semverdiff"
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
		{"releaseAgeCheck", func(cl config.Class) bool { return cl.ReleaseAgeCheck != "" }, releaseAge},
		{"holdoutChecks", func(cl config.Class) bool { return len(cl.HoldoutChecks) > 0 }, holdoutChecks},
		{"oracle", func(cl config.Class) bool { return cl.Oracle != nil }, oracle},
	}
}

// compile and matchesAny are filefence's, shared with the adopt poller.
func compile(pats []string, anchored bool) []*regexp.Regexp { return filefence.Compile(pats, anchored) }

func matchesAny(res []*regexp.Regexp, path string) bool { return filefence.MatchesAny(res, path) }

// The file conditions are filefence's: the poller binds a pull request to a class by the
// same functions.
func allowedFiles(cl config.Class, in ArmInputs) (string, bool) {
	return filefence.AllowedFiles(cl, in.Stats), false
}

func protectedFiles(cl config.Class, in ArmInputs) (string, bool) {
	return filefence.ProtectedFiles(cl, in.Stats), false
}

func modifyOnly(_ config.Class, in ArmInputs) (string, bool) {
	return filefence.ModifyOnly(in.Stats), false
}

func maxPatchFiles(cl config.Class, in ArmInputs) (string, bool) {
	return filefence.MaxPatchFiles(cl, in.Stats), false
}

func maxModifiedLines(cl config.Class, in ArmInputs) (string, bool) {
	return filefence.MaxModifiedLines(cl, in.Stats), false
}

// diffShape: links-only is R52's promise, read on in.Files as it always was, so a diff not read
// yet passes it vacuously (LinksOnly of two empty trees). The semver shapes are computed from the
// diff (MC11) and are held by declared until it is read.
func diffShape(cl config.Class, in ArmInputs) (string, bool) {
	switch cl.DiffShape {
	case "links-only":
		if ok, _ := mdlinks.LinksOnly(in.Files.Base, in.Files.Head); !ok {
			return "class_mismatch", false
		}
	case "semver-patch":
		if ok, _ := semverdiff.Patch(in.Files.Base, in.Files.Head); !ok {
			return "class_mismatch", false
		}
	case "semver-minor":
		if ok, _ := semverdiff.Minor(in.Files.Base, in.Files.Head); !ok {
			return "class_mismatch", false
		}
	}
	return "", false
}

// releaseAge (MC11): the release age is a commit status an adopted login posts (Renovate's
// renovate/stability-days). Not posted yet, or pending, waits; posted by anyone else, or anything
// but success, refuses. An unreadable age is a refusal: the status is read from the rollup, which
// reports "" for a status it does not hold, and a truncated rollup may hold it unseen.
func releaseAge(cl config.Class, in ArmInputs) (string, bool) {
	state, creator := in.Checks.StatusOf(cl.ReleaseAgeCheck)
	if state != "" && !slices.ContainsFunc(cl.Author.Adopt.Logins, func(l string) bool { return forge.SameLogin(creator, l) }) {
		return "release_age_red", false
	}
	switch state {
	case "SUCCESS":
		return "", false
	case "", "PENDING":
		if in.Checks.Truncated && state == "" {
			return "release_age_red", false // a rollup read in part may hold it unseen
		}
		return "release_age_pending", true
	}
	return "release_age_red", false
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

// unreported are the checks not reported as completed, which a wait verdict still waits on (a
// PENDING one counts: it has not finished): the required checks not yet SUCCESS,
// then the class's holdouts with no run of the name that succeeded, in config order, each once.
func unreported(in ArmInputs, cl config.Class) []string {
	var out []string
	add := func(n string) {
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	for _, n := range requiredFor(in.Cfg.RepositoryFor(in.Task.Spec.Repository), in.Task) {
		if i := slices.IndexFunc(in.Checks.Runs, func(c forge.Check) bool { return c.Name == n }); i < 0 || in.Checks.Runs[i].State != "SUCCESS" {
			add(n)
		}
	}
	if cl.ReleaseAgeCheck != "" {
		if state, _ := in.Checks.StatusOf(cl.ReleaseAgeCheck); state != "SUCCESS" {
			add(cl.ReleaseAgeCheck)
		}
	}
	for _, n := range cl.HoldoutChecks {
		ok := slices.ContainsFunc(in.Checks.Runs, func(c forge.Check) bool { return c.Name == n && c.State == "SUCCESS" && c.Conclusion == "SUCCESS" })
		if !ok {
			add(n)
		}
	}
	return out
}

// oracle (MC5, MC6): every finding was a link target in its file on the base and is none on the
// head, and every target the head moved is a finding's, moved to exactly the snapshot that
// finding gave: no same-host move, no other snapshot. A finding with no snapshot has no move.
// diffShape links-only (Validate) has already held the rest of the text and the targets' count.
// Only the head's files are walked here: a deleted file is refused by that shape's count check
// (loop 2 of mdlinks.LinksOnly walks the base's paths too), so that check is not redundant.
func oracle(_ config.Class, in ArmInputs) (string, bool) {
	fs := in.Task.Spec.Findings
	if len(fs) == 0 {
		return "oracle_finding_open", false // a task of an oracle class is its findings: none is no proof
	}
	for _, f := range fs {
		h, ok := in.Files.Head[f.File]
		_, tb := mdlinks.Targets(in.Files.Base[f.File])
		if _, ts := mdlinks.Targets(h); !ok || !slices.Contains(tb, f.URL) || slices.Contains(ts, f.URL) {
			return "oracle_finding_open", false
		}
	}
	for _, p := range slices.Sorted(maps.Keys(in.Files.Head)) {
		_, tb := mdlinks.Targets(in.Files.Base[p])
		_, th := mdlinks.Targets(in.Files.Head[p])
		if len(tb) != len(th) {
			return "oracle_unrequested", false
		}
		for i := range tb {
			if tb[i] == th[i] {
				continue
			}
			j := slices.IndexFunc(fs, func(f v1alpha1.Finding) bool { return f.File == p && f.URL == tb[i] })
			if j < 0 || fs[j].Archive == "" || th[i] != fs[j].Archive {
				return "oracle_unrequested", false
			}
		}
	}
	return "", false
}

// onDiff are the conditions that read the diff, which hold until it is read.
var onDiff = []string{"allowedFiles", "protectedFiles", "modifyOnly", "maxPatchFiles", "maxModifiedLines", "oracle"}

// onDiffFor is whether the condition reads the diff for this class (L8): diffShape does for the
// semver shapes, which would refuse two empty trees, and not for links-only (R52's vacuous pass).
func onDiffFor(c Condition, cl config.Class) bool {
	return slices.Contains(onDiff, c.Key) ||
		c.Key == "diffShape" && (cl.DiffShape == "semver-patch" || cl.DiffShape == "semver-minor")
}

// rowOf names the decision record's string row a condition fills (AM8); the file conditions
// fill Files and Lines, which declared counts whether they are declared or not.
var rowOf = map[string]func(*decision.Rows) *string{
	"diffShape":       func(r *decision.Rows) *string { return &r.Shape },
	"releaseAgeCheck": func(r *decision.Rows) *string { return &r.Age },
	"holdoutChecks":   func(r *decision.Rows) *string { return &r.Holdout },
	"oracle":          func(r *decision.Rows) *string { return &r.Oracle },
}

// declared checks the class's declared conditions in order: the first refusal, or "". rows are
// what it read (AM8): "n/a" for a row the class does not declare, "ok" for one that held, the
// reason for the one that refused ("pending" for a release age still waiting), and "" for a
// declared row it did not reach or a diff row while the diff is unread.
func declared(cl config.Class, in ArmInputs) (rows decision.Rows, reason string, wait bool) {
	rows = decision.Rows{Files: len(in.Stats), MaxFiles: cl.MaxPatchFiles, MaxLines: cl.MaxModifiedLines}
	for _, f := range in.Stats {
		rows.Lines += f.Additions + f.Deletions
	}
	for _, c := range Conditions() {
		if row, ok := rowOf[c.Key]; ok && !c.Declared(cl) {
			*row(&rows) = "n/a"
		}
	}
	for _, c := range Conditions() {
		if !c.Declared(cl) || (!in.DiffRead && onDiffFor(c, cl)) {
			continue
		}
		reason, wait := c.Check(cl, in)
		if row, ok := rowOf[c.Key]; ok {
			*row(&rows) = cmp.Or(reason, "ok")
			if wait && c.Key == "releaseAgeCheck" {
				*row(&rows) = "pending"
			}
		}
		if reason != "" {
			return rows, reason, wait
		}
	}
	return rows, "", false
}
