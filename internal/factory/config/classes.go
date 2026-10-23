// SPDX-License-Identifier: Apache-2.0

package config

import (
	"regexp"
	"slices"
	"strings"
	"time"
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
	// Checks switches the repository's checks on for the class's public implementer and reviewer
	// runs (v3 feature 3, RC8): the claim's spec.loop.checks and the repository's checks.egress.
	Checks bool `json:"checks,omitempty"`
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
	// Oracle makes the class oracle-backed (MC4–MC6): its tasks are a detector's findings, and its
	// gate holds only when the head no longer holds them and changes nothing else.
	Oracle *Oracle `json:"oracle,omitempty"`
	// Monitor watches each merge of the class after it lands (merge gate classes MC7): main's CI
	// for every merge, the detector's next runs for an oracle class, Flux on the factory's cluster,
	// and follow-up fixes for FollowUpWindow. Absent: no monitor, as before.
	Monitor *Monitor `json:"monitor,omitempty"`
	// Author is how the class's pull requests are authored (MC1's author row): absent, the agents
	// App through a task's runs; Adopt, another author's pull requests the factory adopts (MC10).
	Author *Author `json:"author,omitempty"`
	// ReleaseAgeCheck names the commit status that holds the release age (MC11), posted by an
	// adopted login: Renovate's renovate/stability-days.
	ReleaseAgeCheck string `json:"releaseAgeCheck,omitempty"`
}

// Author is a class's author mode.
type Author struct {
	Adopt *Adopt `json:"adopt,omitempty"`
}

// Adopt names the logins whose pull requests are adopted and the head branches they come from
// (gh-aw's allowed-branches); every commit and every push of the branch must be theirs.
type Adopt struct {
	Logins   []string `json:"logins"`
	Branches []string `json:"branches"`
}

// Adopted reports whether the class's pull requests are another author's, adopted (MC10).
func (c Class) Adopted() bool { return c.Author != nil && c.Author.Adopt != nil }

// Monitor is a class's post-merge watch (MC7).
type Monitor struct {
	// OracleRuns is how many scheduled detector runs after the merge must report none of the
	// targets the pull request wrote; 0 reads none. Needs an oracle.
	OracleRuns int `json:"oracleRuns,omitempty"`
	// Flux watches the factory's cluster once its source holds the merge commit.
	Flux *FluxWatch `json:"flux,omitempty"`
	// FollowUpWindow is how long later merges are read for a fix of this one (MC8), and when the
	// monitor's verdict is final.
	FollowUpWindow Duration `json:"followUpWindow"`
}

// FluxWatch names the GitRepository that delivers the repository to the factory's cluster, and
// how long after it reaches the merge commit a Kustomization or HelmRelease turning not Ready is
// blamed on the merge.
type FluxWatch struct {
	Source string   `json:"source"` // namespace/name
	Within Duration `json:"within"`
}

// Namespace splits Source.
func (f FluxWatch) Namespace() string { ns, _, _ := strings.Cut(f.Source, "/"); return ns }

// Name splits Source.
func (f FluxWatch) Name() string { _, n, _ := strings.Cut(f.Source, "/"); return n }

var sourceRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?/[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)

// Oracle is an oracle-backed class's detector: the probe that turns its findings into a task,
// the workflow on main whose runs report them, and the most findings one task holds.
type Oracle struct {
	Probe       string `json:"probe"`
	Workflow    string `json:"workflow"`
	MaxFindings int    `json:"maxFindings"`
}

// MaxFindings bounds oracle.maxFindings: the Task CRD's spec.findings holds at most 16.
const MaxFindings = 16

var workflowRE = regexp.MustCompile(`^[A-Za-z0-9._-]+\.ya?ml$`)

// DiffShapes are the shapes diffShape names.
func DiffShapes() []string { return []string{"links-only", "semver-patch", "semver-minor"} }

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
		c.MaxPatchFiles > 0 || c.MaxModifiedLines > 0 || c.DiffShape != "" || c.Oracle != nil
}

// validateClass reports one class's problems.
func validateClass(name string, cl Class, bad func(string, ...any)) {
	if cl.Live && cl.Shadow {
		bad("class %s: live and shadow never together (R32)", name)
	}
	if m := cl.Monitor; m != nil {
		if w := m.FollowUpWindow.Duration; w < time.Hour || w > 720*time.Hour {
			bad("class %s: monitor.followUpWindow must be 1h..720h", name)
		}
		switch {
		case m.OracleRuns < 0 || m.OracleRuns > 5:
			bad("class %s: monitor.oracleRuns must be 0..5", name)
		case m.OracleRuns > 0 && cl.Oracle == nil:
			bad("class %s: monitor.oracleRuns needs an oracle", name)
		}
		if f := m.Flux; f != nil {
			if !sourceRE.MatchString(f.Source) {
				bad("class %s: monitor.flux.source %q is not namespace/name", name, f.Source)
			}
			if f.Within.Duration < 5*time.Minute || f.Within.Duration > 6*time.Hour {
				bad("class %s: monitor.flux.within must be 5m..6h", name)
			}
		}
	}
	for _, p := range cl.AllowedFiles {
		// The match is unanchored, as policy-bot's is, so an allow list must say where a path
		// begins and ends: "docs/.+" would allow "evil/docs/x".
		if !strings.HasPrefix(p, "^") || !strings.HasSuffix(p, "$") {
			bad("class %s: allowedFiles %q must start with ^ and end with $", name, p)
		}
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
	semver := cl.DiffShape == "semver-patch" || cl.DiffShape == "semver-minor"
	switch {
	case semver && !cl.Adopted():
		bad("class %s: diffShape %s needs author.adopt (MC10)", name, cl.DiffShape)
	case cl.Adopted() && !semver:
		bad("class %s: author.adopt needs diffShape semver-patch or semver-minor", name)
	case semver && !cl.ModifyOnly:
		bad("class %s: a semver shape needs modifyOnly: true", name)
	case cl.DiffShape == "semver-minor" && cl.Live:
		bad("class %s: semver-minor is shadow only (M2)", name)
	case cl.ReleaseAgeCheck != "" && !cl.Adopted():
		bad("class %s: releaseAgeCheck needs author.adopt", name)
	}
	if cl.Adopted() {
		a := cl.Author.Adopt
		if len(a.Logins) == 0 || len(a.Branches) == 0 {
			bad("class %s: author.adopt needs logins and branches", name)
		}
		for _, b := range a.Branches {
			if _, err := regexp.Compile(b); err != nil || !strings.HasPrefix(b, "^") {
				bad("class %s: author.adopt.branches %q must start with ^", name, b)
			}
		}
	}
	if o := cl.Oracle; o != nil {
		switch {
		case o.Probe != "dead-links":
			bad("class %s: oracle.probe %q is not dead-links", name, o.Probe)
		case !workflowRE.MatchString(o.Workflow):
			bad("class %s: oracle.workflow %q is not a workflow file name", name, o.Workflow)
		case o.MaxFindings < 1 || o.MaxFindings > MaxFindings:
			bad("class %s: oracle.maxFindings must be 1..%d", name, MaxFindings)
		case len(cl.HoldoutChecks) == 0:
			bad("class %s: an oracle needs holdoutChecks, or the head goes unproven (MC5)", name)
		case cl.DiffShape != "links-only":
			bad("class %s: an oracle reads link targets: diffShape must be links-only", name)
		case !cl.ModifyOnly:
			bad("class %s: an oracle needs modifyOnly: true, or an added or deleted empty file would pass", name)
		case len(cl.AllowedFiles) == 0:
			bad("class %s: an oracle needs allowedFiles, or the probe drops every finding", name)
		case cl.MaxModifiedLines > 0 && cl.MaxModifiedLines < 2*o.MaxFindings:
			bad("class %s: maxModifiedLines %d is under 2 x oracle.maxFindings (%d): one retarget changes two lines", name, cl.MaxModifiedLines, 2*o.MaxFindings)
		}
	}
	seen := map[string]bool{}
	for _, h := range cl.HoldoutChecks {
		if h == "" || seen[h] {
			bad("class %s: holdoutChecks names %q empty or twice", name, h)
		}
		seen[h] = true
	}
}
