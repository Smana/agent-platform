// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"slices"
	"strings"
	"testing"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/forge"
)

// head is the pull request's head at the decision and oldHead a head it was before: full
// 40-character shas, which is what the room reports (R52).
var (
	head    = strings.Repeat("a", 40)
	oldHead = strings.Repeat("b", 40)
)

var required = []string{"Pre-commit checks", "Kubernetes validation"}

const testFile = "website/content/docs/a.md"

func armCfg() *config.Config {
	return &config.Config{Maintainers: []string{"Smana"}, AgentsLogin: "ogenki-agents[bot]",
		Templates: map[string]config.Template{"solo": {Roles: []string{"implementer"}}, "pair": {Roles: []string{"implementer", "reviewer"}}},
		Classes:   map[string]config.Class{"docs-links": {Live: true}, "revert": {Live: true}, "docs": {}},
		Merge:     config.Merge{RequiredChecks: required, LeakScanCheck: "Security scanning", PolicyBotLogin: "ogenki-merge-gate[bot]", AutoMergesPerDay: 10}}
}

func green(policy string) forge.Checks {
	return forge.Checks{Runs: []forge.Check{{Name: "Pre-commit checks", State: "SUCCESS"}, {Name: "Kubernetes validation", State: "SUCCESS"}},
		Statuses: []forge.Status{{Context: "policy-bot: main", State: policy, Creator: "ogenki-merge-gate[bot]"}}}
}

func armTask(class, template, verdict string) *v1alpha1.Task {
	return &v1alpha1.Task{Spec: v1alpha1.TaskSpec{PredictedClass: class, Template: template},
		Status: v1alpha1.TaskStatus{Verdict: verdict, Runs: []v1alpha1.RunRecord{
			{ID: "7f3cq2xz", Role: "implementer", Trigger: "initial"},
			{ID: "3buqdlot", Role: "implementer", Trigger: "review"}}}}
}

func agentPR() forge.PR {
	return forge.PR{Author: "ogenki-agents[bot]", HeadSHA: head, HeadMessage: "docs: fix\n\nAgent-Run: 7f3cq2xz"}
}

// changed is the forge.Files read of a diff of one file.
func changed(base, headText string) struct{ Base, Head map[string]string } {
	return struct{ Base, Head map[string]string }{
		Base: map[string]string{testFile: base}, Head: map[string]string{testFile: headText}}
}

// The amendment (external reviews R02, R03; ruling R52), written before the brief's table
// because it supersedes that table's trailer-only rule: the head must be the commit a run of
// the task reported, a verifier's approve must be of that head, and the legitimate path
// decides but is not armed while docs-links is shadow (R32).
func TestHeadAndVerdictMustBeFresh(t *testing.T) {
	cfg := armCfg()
	cfg.Classes["docs-links"] = config.Class{Shadow: true}
	approve := func(sha string) []v1alpha1.RunRecord {
		return []v1alpha1.RunRecord{{ID: "r3vnwr3v", Role: "reviewer", Trigger: "review", Verdict: "approve", HeadSHA: sha}}
	}
	for name, c := range map[string]struct {
		task            *v1alpha1.Task
		reported        string
		verifs          []v1alpha1.RunRecord
		verdict, reason string
	}{
		// R02: run D pushed H with A's trailer. The trailer is one of the task's own runs,
		// so the old rule would arm it — but no handoff ever named H: the room last reported
		// another commit, and an unreported head is never armed.
		"the pushed head was never reported": {armTask("docs-links", "solo", ""), oldHead, nil, "human", "head_unreported"},
		"no head was reported at all":        {armTask("docs-links", "solo", ""), "", nil, "human", "head_unreported"},
		// R03: the reviewer approved H1; H2 was pushed before the decision.
		"the approve is of a superseded head": {armTask("docs-links", "pair", "approve"), head, approve(oldHead), "human", "verdict_stale"},
		// The legitimate path, for a pair (a fresh approve) and a solo (docs-links is solo;
		// nothing verifies it): decided exactly like a live class, and shadow says would arm.
		"legitimate pair": {armTask("docs-links", "pair", "approve"), head, approve(head), "shadow", "shadow_would_arm"},
		"legitimate solo": {armTask("docs-links", "solo", ""), head, nil, "shadow", "shadow_would_arm"},
	} {
		t.Run(name, func(t *testing.T) {
			d := DecideArm(ArmInputs{PR: agentPR(), Checks: green("SUCCESS"), Task: c.task, Cfg: cfg,
				ReportedHead: c.reported, Verifiers: c.verifs})
			if d.Verdict != c.verdict || d.Reason != c.reason || d.Matched != "docs-links" {
				t.Fatalf("got %+v, want %s/%s/docs-links", d, c.verdict, c.reason)
			}
		})
	}
}

// A closing fence ends its block: each block is its own range, and only an unclosed one runs
// to the end of the text (cloud-native-ref#2517).
func TestFencesOf(t *testing.T) {
	for name, c := range map[string]struct {
		s    string
		want [][2]int
	}{
		"two blocks":            {"```\nx\n```\ntext\n```\ny\n```\n", [][2]int{{0, 9}, {15, 24}}},
		"indented blocks":       {"- a\n  ```\n  x\n  ```\n- b\n\t```\n\t```", [][2]int{{4, 19}, {24, 33}}},
		"unclosed at EOF":       {"a\n```\nb", [][2]int{{2, 7}}},
		"closed, then unclosed": {"```\n```\nt\n```\nu", [][2]int{{0, 7}, {10, 15}}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := fencesOf(c.s); !slices.Equal(got, c.want) {
				t.Fatalf("fencesOf = %v, want %v", got, c.want)
			}
		})
	}
}

// R52's LinksOnly: a docs-links or revert diff may move link targets and nothing else.
func TestLinksOnly(t *testing.T) {
	one := func(s string) map[string]string { return map[string]string{testFile: s} }
	for name, c := range map[string]struct {
		base, headText string
		ok             bool
	}{
		"a relative retarget":             {"see [the guide](docs/old.md).", "see [the guide](docs/new.md).", true},
		"a retarget within the host":      {"[a](https://example.com/old)", "[a](https://example.com/new)", true},
		"a retarget to the archive":       {"[a](https://example.com/gone)", "[a](https://web.archive.org/web/2020/https://example.com/gone)", true},
		"a definition retarget":           {"[a]: https://example.com/old\n[a] and text", "[a]: https://example.com/new\n[a] and text", true},
		"a title unchanged, target moved": {`[a](docs/old.md "The Guide")`, `[a](docs/new.md "The Guide")`, true},
		"relative to absolute":            {"[a](docs/x.md)", "[a](https://example.com/x)", false},
		"a protocol-relative jump":        {"[a](docs/x.md)", "[a](//evil.com/x)", false},
		"a fenced retarget":               {"```\n[a](docs/old.md)\n```", "```\n[a](docs/new.md)\n```", false},
		"an injected script":              {"plain text.", "plain text <script>alert(1)</script>.", false},
		"a changed fenced command":        {"```\ncurl https://example.com/old\n```", "```\ncurl https://example.com/new\n```", false},
		"a reworded sentence":             {"The link is broken.", "The link has rotted.", false},
		"a new external host":             {"[a](https://example.com/x)", "[a](https://evil.example/x)", false},
		"a javascript target":             {"[a](https://example.com/x)", "[a](javascript:alert(1))", false},
		"a link added":                    {"text.", "text. [a](new.md)", false},
		// cloud-native-ref#2517: a code block above the links must not fence everything after it.
		"a retarget after a block":           {"```\nx\n```\n[a](docs/old.md)", "```\nx\n```\n[a](docs/new.md)", true},
		"a retarget between two blocks":      {"```\nx\n```\n[a](old.md)\n```\ny\n```", "```\nx\n```\n[a](new.md)\n```\ny\n```", true},
		"a retarget after two blocks":        {"```\nx\n```\n```\ny\n```\n[a](old.md)", "```\nx\n```\n```\ny\n```\n[a](new.md)", true},
		"a retarget after a list's block":    {"- step:\n  ```sh\n  ls\n  ```\n[a](old.md)", "- step:\n  ```sh\n  ls\n  ```\n[a](new.md)", true},
		"a retarget in a list's block":       {"- step:\n  ```\n  [a](old.md)\n  ```\n", "- step:\n  ```\n  [a](new.md)\n  ```\n", false},
		"a retarget after an unclosed fence": {"```\nx\n[a](old.md)", "```\nx\n[a](new.md)", false},
	} {
		t.Run(name, func(t *testing.T) {
			ok, why := LinksOnly(one(c.base), one(c.headText))
			if ok != c.ok {
				t.Fatalf("ok = %v (%s), want %v", ok, why, c.ok)
			}
			if ok && why != "" {
				t.Fatalf("why = %q on a links-only diff", why)
			}
		})
	}
	t.Run("files", func(t *testing.T) {
		if ok, _ := LinksOnly(map[string]string{"a.md": "keep", "b.md": "[x](old.md)"},
			map[string]string{"a.md": "keep", "b.md": "[x](new.md)"}); !ok {
			t.Fatal("an unchanged file beside a retarget is links-only")
		}
		if ok, _ := LinksOnly(map[string]string{"a.md": "x"}, map[string]string{"a.md": "x", "b.md": "[y](z.md)"}); ok {
			t.Fatal("an added file is not a link retarget")
		}
		if ok, _ := LinksOnly(nil, nil); !ok {
			t.Fatal("no changed file contradicts nothing")
		}
	})
}

func TestDecideArm(t *testing.T) {
	approved := agentPR()
	approved.Reviews = []forge.Review{{Author: "Smana", State: "APPROVED"}}
	foreign := agentPR()
	foreign.HeadMessage = "docs: sneak\n\nAgent-Run: zzzzzzzz"
	byOwner := agentPR()
	byOwner.Author = "Smana"
	red := green("SUCCESS")
	red.Runs[1].State = "FAILURE"
	errored := green("SUCCESS")
	errored.Runs[1].State = "ERROR" // a state the merger's rollup never emits: recognized or not, it is not green
	pending := green("SUCCESS")
	pending.Runs = pending.Runs[:1] // a required check has not reported yet
	forged := green("SUCCESS")
	forged.Statuses[0].Creator = "ogenki-agents[bot]"
	scanRed := green("SUCCESS")
	scanRed.Runs = append(scanRed.Runs, forge.Check{Name: "Security scanning", State: "FAILURE"})
	for name, c := range map[string]struct {
		in                       ArmInputs
		verdict, reason, matched string
	}{
		"all conditions": {ArmInputs{PR: agentPR(), Checks: green("SUCCESS"), Task: armTask("docs-links", "solo", ""),
			ReportedHead: head, Files: changed("[x](old.md)", "[x](new.md)")}, "arm", "", "docs-links"},
		// v2 D7: every other condition holds, but the fence names another factory: a human merges.
		"not this factory's repository": {ArmInputs{PR: agentPR(), Checks: green("SUCCESS"), Task: armTask("docs-links", "solo", ""),
			ReportedHead: head, Files: changed("[x](old.md)", "[x](new.md)"), NotOwned: true}, "human", "not_owned", ""},
		"CI red":                   {ArmInputs{PR: agentPR(), Checks: red, Task: armTask("docs-links", "solo", "")}, "ci_red", "ci_red", ""},
		"CI errored":               {ArmInputs{PR: agentPR(), Checks: errored, Task: armTask("docs-links", "solo", "")}, "ci_red", "ci_red", ""},
		"CI still running":         {ArmInputs{PR: agentPR(), Checks: pending, Task: armTask("docs-links", "solo", "")}, "wait", "ci_pending", ""},
		"secret scan red":          {ArmInputs{PR: agentPR(), Checks: scanRed, Task: armTask("docs-links", "solo", "")}, "escalate", "secret_scan_red", ""},
		"gate path":                {ArmInputs{PR: agentPR(), Checks: green("ERROR"), Task: armTask("docs-links", "solo", "")}, "human", "gate_path", "gate"},
		"diff not the class":       {ArmInputs{PR: agentPR(), Checks: green("PENDING"), Task: armTask("docs-links", "solo", "")}, "human", "policy_pending", "review"},
		"a forged status":          {ArmInputs{PR: agentPR(), Checks: forged, Task: armTask("docs-links", "solo", "")}, "human", "policy_absent", ""},
		"a non-live class matched": {ArmInputs{PR: agentPR(), Checks: green("SUCCESS"), Task: armTask("docs", "pair", "approve")}, "human", "class_mismatch", "live"},
		"maintainer approved":      {ArmInputs{PR: approved, Checks: green("SUCCESS"), Task: armTask("docs-links", "solo", "")}, "human", "maintainer_approved", "review"},
		"not an agent's PR":        {ArmInputs{PR: byOwner, Checks: green("SUCCESS"), Task: armTask("docs-links", "solo", "")}, "human", "not_agent_authored", ""},
		"no approving verdict":     {ArmInputs{PR: agentPR(), Checks: green("SUCCESS"), Task: armTask("docs-links", "pair", "none")}, "human", "no_approving_verdict", "docs-links"},
		// R52, in table form: each gate is the reason, not the trailer's match.
		"SC-14 foreign trailer": {ArmInputs{PR: foreign, Checks: green("SUCCESS"), Task: armTask("docs-links", "solo", ""),
			ReportedHead: head}, "human", "foreign_trailer", "docs-links"},
		"the head was never reported": {ArmInputs{PR: agentPR(), Checks: green("SUCCESS"), Task: armTask("docs-links", "solo", "")}, "human", "head_unreported", "docs-links"},
		"an approve of a superseded head": {ArmInputs{PR: agentPR(), Checks: green("SUCCESS"), Task: armTask("docs-links", "pair", "approve"),
			ReportedHead: head, Verifiers: []v1alpha1.RunRecord{{ID: "r3vnwr3v", Role: "reviewer", Trigger: "review", Verdict: "approve", HeadSHA: oldHead}}},
			"human", "verdict_stale", "docs-links"},
		"docs-links moved beyond links": {ArmInputs{PR: agentPR(), Checks: green("SUCCESS"), Task: armTask("docs-links", "solo", ""),
			ReportedHead: head, Files: changed("text.", "text <script>alert(1)</script>.")}, "human", "class_mismatch", "docs-links"},
		"revert moved beyond links": {ArmInputs{PR: agentPR(), Checks: green("SUCCESS"), Task: armTask("revert", "solo", ""),
			ReportedHead: head, Files: changed("The link is broken.", "The link has rotted.")}, "human", "class_mismatch", "revert"},
		"class paused": {ArmInputs{PR: agentPR(), Checks: green("SUCCESS"), Task: armTask("docs-links", "solo", ""),
			ReportedHead: head, Paused: true}, "human", "class_paused", "docs-links"},
		"daily cap": {ArmInputs{PR: agentPR(), Checks: green("SUCCESS"), Task: armTask("docs-links", "solo", ""),
			ReportedHead: head, ArmedToday: 10}, "human", "auto_merge_cap", "docs-links"},
	} {
		t.Run(name, func(t *testing.T) {
			c.in.Cfg = armCfg()
			d := DecideArm(c.in)
			if d.Verdict != c.verdict || d.Reason != c.reason || d.Matched != c.matched {
				t.Errorf("%s: got %+v, want %s/%s/%s", name, d, c.verdict, c.reason, c.matched)
			}
		})
	}
}

// R32 (owner, 2026-09-27): before the wave a class is shadow, decided exactly like a live one and
// never armed; a foreign trailer is never even "would arm".
func TestAShadowClassWouldArmAndNeverArms(t *testing.T) {
	cfg := armCfg()
	cfg.Classes["docs-links"] = config.Class{Shadow: true}
	if d := DecideArm(ArmInputs{PR: agentPR(), Checks: green("SUCCESS"), Task: armTask("docs-links", "solo", ""),
		Cfg: cfg, ReportedHead: head}); d.Verdict != "shadow" || d.Reason != "shadow_would_arm" || d.Matched != "docs-links" {
		t.Fatalf("%+v", d)
	}
	foreign := agentPR()
	foreign.HeadMessage = "docs: sneak\n\nAgent-Run: zzzzzzzz"
	if d := DecideArm(ArmInputs{PR: foreign, Checks: green("SUCCESS"), Task: armTask("docs-links", "solo", ""),
		Cfg: cfg, ReportedHead: head}); d.Reason != "foreign_trailer" {
		t.Fatalf("%+v", d)
	}
}

// The merge gate reads the task's repository (v2 D1): the same PR arms on A, where docs-links is
// live, would only on B, where it is shadow, and waits on C, whose own required check has not run.
func TestDecideArmReadsTheTasksRepository(t *testing.T) {
	cfg := armCfg()
	a := config.Repository{Name: "Smana/a", Maintainers: cfg.Maintainers, Classes: cfg.Classes, Merge: cfg.Merge}
	b := a
	b.Name, b.Classes = "Smana/b", map[string]config.Class{"docs-links": {Shadow: true}, "revert": {}}
	strict := b
	strict.Name, strict.Merge.RequiredChecks = "Smana/c", append(slices.Clone(required), "B's own check")
	cfg.Maintainers, cfg.Classes, cfg.Merge = nil, nil, config.Merge{}
	cfg.Repositories = []config.Repository{a, b, strict}
	for repo, want := range map[string]string{"Smana/a": "arm", "Smana/b": "shadow", "Smana/c": "wait"} {
		tk := armTask("docs-links", "solo", "")
		tk.Spec.Repository = repo
		d := DecideArm(ArmInputs{PR: agentPR(), Checks: green("SUCCESS"), Task: tk, Cfg: cfg, ReportedHead: head,
			Files: changed("[x](old.md)", "[x](new.md)")})
		if d.Verdict != want {
			t.Errorf("%s: %+v, want %s", repo, d, want)
		}
	}
}
