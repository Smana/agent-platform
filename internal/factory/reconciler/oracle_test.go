// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"testing"

	v1alpha1 "github.com/Smana/maku/api/factory/v1alpha1"
	"github.com/Smana/maku/internal/factory/config"
	"github.com/Smana/maku/internal/factory/forge"
)

const (
	gone    = "https://example.com/gone"
	archive = "https://web.archive.org/web/20261011195309/https://example.com/gone"
	page    = "[a](https://example.com/gone) and [b](https://other.example/kept)."
)

// oracleInputs: a dead-links task of one finding, fixed on the head by a retarget to its archive.
func oracleInputs(headText string) ArmInputs {
	in := gateInputs()
	cl := declaredClass()
	cl.Oracle = &config.Oracle{Probe: "dead-links", Workflow: "dead-links.yaml", MaxFindings: 10}
	in.Cfg.Classes["dead-links"] = cl
	in.Task.Spec.Findings = []v1alpha1.Finding{{File: testFile, URL: gone, Archive: archive}}
	in.Files = changed(page, headText)
	in.Stats = []forge.FileStat{{Path: testFile, Status: "modified", Additions: 1, Deletions: 1}}
	return in
}

// Offline proof 9: the oracle condition on fixtures, each with its reason.
func TestTheOracle(t *testing.T) {
	for name, c := range map[string]struct {
		head, reason string
	}{
		"to the snapshot given":     {"[a](" + archive + ") and [b](https://other.example/kept).", ""},
		"within the host":           {"[a](https://example.com/moved) and [b](https://other.example/kept).", "oracle_unrequested"},
		"the finding left":          {page, "oracle_finding_open"},
		"another target moved":      {"[a](" + archive + ") and [b](https://other.example/new).", "oracle_unrequested"},
		"another snapshot":          {"[a](https://web.archive.org/web/2020/https://example.com/gone) and [b](https://other.example/kept).", "oracle_unrequested"},
		"the finding's file unread": {"", "oracle_finding_open"},
	} {
		t.Run(name, func(t *testing.T) {
			in := oracleInputs(c.head)
			if c.head == "" {
				in.Files = changed("x", "x")
				in.Files.Base, in.Files.Head = map[string]string{"docs/other.md": "x"}, map[string]string{"docs/other.md": "x"}
			}
			d := DecideArm(in)
			if (c.reason == "" && d.Verdict != "arm") || (c.reason != "" && d.Reason != c.reason) {
				t.Fatalf("got %+v, want %q", d, c.reason)
			}
		})
	}
	in := oracleInputs(page)
	in.Task.Spec.Findings = nil
	if d := DecideArm(in); d.Reason != "oracle_finding_open" {
		t.Fatalf("an oracle task without findings proves nothing: %+v", d)
	}
}

const (
	orgOld = "https://github.com/org/old"
	orgNew = "https://web.archive.org/web/2026/https://github.com/org/old"
)

// MC6 ruling: an oracle retarget equals the finding's own snapshot, byte for byte; a same-host
// move, a case-folded host or another snapshot is another target moved.
func TestTheOracleRetargetIsTheSnapshotGiven(t *testing.T) {
	for name, c := range map[string]struct{ to, reason string }{
		"the snapshot given":         {orgNew, ""},
		"another account":            {"https://github.com/attacker/x", "oracle_unrequested"},
		"another snapshot":           {"https://web.archive.org/web/2020/https://github.com/org/old", "oracle_unrequested"},
		"the host in other case":     {"https://GitHub.com/org/new", "oracle_unrequested"},
		"the snapshot in a case":     {"https://web.archive.org/WEB/2026/https://github.com/org/old", "oracle_unrequested"},
		"a finding with no snapshot": {"", "oracle_unrequested"},
	} {
		t.Run(name, func(t *testing.T) {
			in := oracleInputs("")
			in.Files = changed("[a]("+orgOld+")", "[a]("+c.to+")")
			in.Task.Spec.Findings = []v1alpha1.Finding{{File: testFile, URL: orgOld, Archive: orgNew}}
			if name == "a finding with no snapshot" {
				in.Task.Spec.Findings[0].Archive = ""
				in.Files = changed("[a]("+orgOld+")", "[a](https://github.com/org/new)")
			}
			d := DecideArm(in)
			if (c.reason == "" && d.Verdict != "arm") || (c.reason != "" && d.Reason != c.reason) {
				t.Fatalf("got %+v, want %q", d, c.reason)
			}
		})
	}
}

// A deleted, renamed or added file is refused earlier, by the links-only shape (class_mismatch).
// A finding counts as fixed only if the base held it and the head does not; every finding does.
func TestTheOracleEveryFindingMustBeFixed(t *testing.T) {
	const a, b = "https://example.com/a", "https://example.com/b"
	arch := func(u string) string { return "https://web.archive.org/web/2026/" + u }
	fnd := func(file, u string) v1alpha1.Finding { return v1alpha1.Finding{File: file, URL: u, Archive: arch(u)} }
	for name, c := range map[string]struct {
		fs         []v1alpha1.Finding
		base, head map[string]string
		stats      []forge.FileStat
		reason     string
	}{
		"both fixed, one file": {[]v1alpha1.Finding{fnd("a.md", a), fnd("a.md", b)},
			map[string]string{"a.md": "[x](" + a + ") [y](" + b + ")"}, map[string]string{"a.md": "[x](" + arch(a) + ") [y](" + arch(b) + ")"}, nil, ""},
		"a stale finding: not in the base": {[]v1alpha1.Finding{fnd("a.md", a), fnd("a.md", "https://example.com/stale")},
			map[string]string{"a.md": "[x](" + a + ")"}, map[string]string{"a.md": "[x](" + arch(a) + ")"}, nil, "oracle_finding_open"},
		"two files, one fixed": {[]v1alpha1.Finding{fnd("a.md", a), fnd("b.md", b)},
			map[string]string{"a.md": "[x](" + a + ")", "b.md": "[y](" + b + ")"}, map[string]string{"a.md": "[x](" + arch(a) + ")", "b.md": "[y](" + b + ")"}, nil, "oracle_finding_open"},
		"a deleted finding file": {[]v1alpha1.Finding{fnd("a.md", a)},
			map[string]string{"a.md": "[x](" + a + ")"}, map[string]string{}, nil, "class_mismatch"},
		"a renamed finding file": {[]v1alpha1.Finding{fnd("a.md", a)},
			map[string]string{"a.md": "[x](" + a + ")"}, map[string]string{"c.md": "[x](" + arch(a) + ")"}, nil, "class_mismatch"},
		"an added file with links": {[]v1alpha1.Finding{fnd("a.md", a)},
			map[string]string{"a.md": "[x](" + a + ")"}, map[string]string{"a.md": "[x](" + arch(a) + ")", "n.md": "[z](https://evil.example/)"}, nil, "class_mismatch"},
	} {
		t.Run(name, func(t *testing.T) {
			in := oracleInputs("")
			in.Task.Spec.Findings = c.fs
			in.Files.Base, in.Files.Head = c.base, c.head
			if c.stats != nil {
				in.Stats = c.stats
			}
			if d := DecideArm(in); (c.reason == "" && d.Verdict != "arm") || (c.reason != "" && d.Reason != c.reason) {
				t.Fatalf("got %+v, want %q", d, c.reason)
			}
		})
	}
}
