// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"testing"

	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/forge"
)

// declaredClass is a class declaring every PR 1 condition, as dead-links will (MC1).
func declaredClass() config.Class {
	return config.Class{Live: true, AllowedFiles: []string{`^(docs|website/content)/.+\.md$`},
		ProtectedFiles: []string{`^docs/(superpowers|specs)/`}, ModifyOnly: true, MaxPatchFiles: 3, MaxModifiedLines: 20,
		DiffShape: "links-only", HoldoutChecks: []string{"External links 🔗"}, MaxPerDay: 1}
}

// gateInputs passes every condition of declaredClass: a green holdout from GitHub Actions and a
// one-line retarget, its diff read.
func gateInputs() ArmInputs {
	checks := green("SUCCESS")
	checks.Runs = append(checks.Runs, forge.Check{Name: "External links 🔗", State: "SUCCESS", App: forge.ActionsApp, Conclusion: "SUCCESS"})
	in := ArmInputs{PR: agentPR(), Checks: checks, Task: armTask("dead-links", "solo", ""), ReportedHead: head,
		Files: changed("[x](https://example.com/old)", "[x](https://example.com/new)"), DiffRead: true,
		Stats: []forge.FileStat{{Path: testFile, Status: "modified", Additions: 1, Deletions: 1}}}
	in.Cfg = armCfg()
	in.Cfg.Classes["dead-links"] = declaredClass()
	return in
}

// Offline proof 2: each condition, failed alone, yields exactly its reason.
func TestEachDeclaredConditionRefusesAlone(t *testing.T) {
	if d := DecideArm(gateInputs()); d.Verdict != "arm" || d.Matched != "dead-links" {
		t.Fatalf("every condition holds: %+v", d)
	}
	for name, c := range map[string]struct {
		mutate          func(*ArmInputs)
		verdict, reason string
	}{
		"a file outside allowedFiles": {func(in *ArmInputs) {
			in.Stats = append(in.Stats, forge.FileStat{Path: "README.md", Status: "modified", Additions: 1})
		}, "human", "file_not_allowed"},
		"a protected file": {func(in *ArmInputs) { in.Stats[0].Path = "docs/specs/x.md" }, "human", "file_protected"},
		"an added file":    {func(in *ArmInputs) { in.Stats[0].Status = "added" }, "human", "file_added_or_deleted"},
		"a renamed file": {func(in *ArmInputs) { in.Stats[0].Status, in.Stats[0].Previous = "renamed", "docs/b.md" },
			"human", "file_added_or_deleted"},
		"four files": {func(in *ArmInputs) {
			for _, p := range []string{"docs/b.md", "docs/c.md", "docs/d.md"} {
				in.Stats = append(in.Stats, forge.FileStat{Path: p, Status: "modified"})
			}
		}, "human", "patch_too_large"},
		"21 lines":            {func(in *ArmInputs) { in.Stats[0].Additions = 20 }, "human", "patch_too_large"},
		"beyond link targets": {func(in *ArmInputs) { in.Files = changed("text.", "text, reworded.") }, "human", "class_mismatch"},
		"the holdout red":     {func(in *ArmInputs) { in.Checks.Runs[2].State = "FAILURE" }, "human", "holdout_red"},
		"the holdout skipped": {func(in *ArmInputs) { in.Checks.Runs[2].Conclusion = "SKIPPED" }, "human", "holdout_red"},
		"the holdout neutral": {func(in *ArmInputs) { in.Checks.Runs[2].Conclusion = "NEUTRAL" }, "human", "holdout_red"},
		"the holdout twice, the second red": {func(in *ArmInputs) {
			in.Checks.Runs = append(in.Checks.Runs, forge.Check{Name: "External links 🔗", State: "FAILURE", App: forge.ActionsApp, Conclusion: "FAILURE"})
		}, "human", "holdout_red"},
		"a rename from a protected file": {func(in *ArmInputs) {
			in.Stats[0].Status, in.Stats[0].Previous = "renamed", "docs/specs/x.md"
			in.Cfg.Classes["dead-links"] = withoutModifyOnly(in.Cfg.Classes["dead-links"])
		}, "human", "file_protected"},
		"the holdout foreign":  {func(in *ArmInputs) { in.Checks.Runs[2].App = 1 }, "human", "holdout_red"},
		"the holdout running":  {func(in *ArmInputs) { in.Checks.Runs[2].State = "PENDING" }, "wait", "holdout_pending"},
		"the holdout not seen": {func(in *ArmInputs) { in.Checks.Runs = in.Checks.Runs[:2] }, "wait", "holdout_pending"},
		"the class's day":      {func(in *ArmInputs) { in.ClassArmedToday = 1 }, "human", "auto_merge_cap"},
	} {
		t.Run(name, func(t *testing.T) {
			in := gateInputs()
			c.mutate(&in)
			if d := DecideArm(in); d.Verdict != c.verdict || d.Reason != c.reason || d.Matched != "dead-links" {
				t.Fatalf("got %+v, want %s/%s/dead-links", d, c.verdict, c.reason)
			}
		})
	}
}

// The conditions on the diff hold until it is read: the first pass of awaitingCI decides
// without it, and only a decision that would arm or shadow reads it.
func TestDiffConditionsWaitForTheDiff(t *testing.T) {
	in := gateInputs()
	in.Stats, in.DiffRead = []forge.FileStat{{Path: "README.md", Status: "added", Additions: 99}}, false
	if d := DecideArm(in); d.Verdict != "arm" {
		t.Fatalf("an unread diff holds every condition on it: %+v", d)
	}
	in.DiffRead = true
	if d := DecideArm(in); d.Reason != "file_not_allowed" {
		t.Fatalf("read, it decides: %+v", d)
	}
}

// The base holds for a class that declares nothing: docs is label-only, so class_mismatch/live
// as today; a declared class with nothing but a switch is decided by the base alone.
func TestTheBaseHoldsForAClassThatDeclaresNothing(t *testing.T) {
	in := gateInputs()
	in.Cfg.Classes["dead-links"] = config.Class{Shadow: true}
	in.Stats, in.Files = []forge.FileStat{{Path: "README.md", Status: "added", Additions: 99}}, changed("a", "b")
	if d := DecideArm(in); d.Verdict != "shadow" || d.Reason != "shadow_would_arm" {
		t.Fatalf("no declared condition: the base alone decides, %+v", d)
	}
	in.Checks.Runs[1].State = "FAILURE"
	if d := DecideArm(in); d.Verdict != "ci_red" {
		t.Fatalf("the base still refuses: %+v", d)
	}
}

func withoutModifyOnly(cl config.Class) config.Class {
	cl.ModifyOnly = false
	return cl
}

// ReadsDiff and the conditions held until the diff is read are one list: a class declaring any
// condition on the diff must get its second pass, or the condition passes vacuously for ever.
func TestEveryConditionOnTheDiffMakesTheClassReadIt(t *testing.T) {
	declare := map[string]config.Class{
		"allowedFiles":     {AllowedFiles: []string{"^a$"}},
		"protectedFiles":   {ProtectedFiles: []string{"a"}},
		"modifyOnly":       {ModifyOnly: true},
		"maxPatchFiles":    {MaxPatchFiles: 1},
		"maxModifiedLines": {MaxModifiedLines: 1},
	}
	for _, key := range onDiff {
		cl, ok := declare[key]
		if !ok {
			t.Fatalf("%s is held until the diff is read: add it here", key)
		}
		if !cl.ReadsDiff("x") {
			t.Errorf("%s declared, and ReadsDiff is false: the diff would never be read", key)
		}
	}
}

// allowedFiles matches whole paths: an alternation that passes the ^…$ shape check anchors each
// branch on one side only, and must not allow docs/a.md.sh.
func TestAllowedFilesAnchorsTheWholePattern(t *testing.T) {
	in := gateInputs()
	cl := declaredClass()
	cl.AllowedFiles = []string{`^docs/.+\.md|website/.+\.md$`}
	in.Cfg.Classes["dead-links"] = cl
	in.Stats[0].Path = "docs/a.md.sh"
	if d := DecideArm(in); d.Reason != "file_not_allowed" {
		t.Fatalf("a branch anchored on one side allowed %s: %+v", in.Stats[0].Path, d)
	}
	for _, p := range []string{"docs/a.md", "website/b.md"} {
		in.Stats[0].Path = p
		if d := DecideArm(in); d.Verdict != "arm" {
			t.Fatalf("%s is allowed: %+v", p, d)
		}
	}
}
