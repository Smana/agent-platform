// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"slices"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/Smana/maku/api/factory/v1alpha1"
	"github.com/Smana/maku/internal/factory/config"
	"github.com/Smana/maku/internal/factory/forge"
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
		"CI red":               {ArmInputs{PR: agentPR(), Checks: red, Task: armTask("docs-links", "solo", "")}, "ci_red", "ci_red", ""},
		"CI errored":           {ArmInputs{PR: agentPR(), Checks: errored, Task: armTask("docs-links", "solo", "")}, "ci_red", "ci_red", ""},
		"CI still running":     {ArmInputs{PR: agentPR(), Checks: pending, Task: armTask("docs-links", "solo", "")}, "wait", "ci_pending", ""},
		"secret scan red":      {ArmInputs{PR: agentPR(), Checks: scanRed, Task: armTask("docs-links", "solo", "")}, "escalate", "secret_scan_red", ""},
		"gate path":            {ArmInputs{PR: agentPR(), Checks: green("ERROR"), Task: armTask("docs-links", "solo", "")}, "human", "gate_path", "gate"},
		"diff not the class":   {ArmInputs{PR: agentPR(), Checks: green("PENDING"), Task: armTask("docs-links", "solo", "")}, "human", "policy_pending", "review"},
		"a forged status":      {ArmInputs{PR: agentPR(), Checks: forged, Task: armTask("docs-links", "solo", "")}, "human", "policy_absent", ""},
		"an unarmed class":     {ArmInputs{PR: agentPR(), Checks: green("SUCCESS"), Task: armTask("docs", "pair", "approve")}, "human", "class_unarmed", ""},
		"maintainer approved":  {ArmInputs{PR: approved, Checks: green("SUCCESS"), Task: armTask("docs-links", "solo", "")}, "human", "maintainer_approved", "review"},
		"not an agent's PR":    {ArmInputs{PR: byOwner, Checks: green("SUCCESS"), Task: armTask("docs-links", "solo", "")}, "human", "not_agent_authored", ""},
		"no approving verdict": {ArmInputs{PR: agentPR(), Checks: green("SUCCESS"), Task: armTask("docs-links", "pair", "none")}, "human", "no_approving_verdict", "docs-links"},
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

// Human path AM4: a shadow class arms nothing, so the breaker and the caps block nothing: it
// records shadow_would_arm and the decision says what a live class would have met.
func TestAShadowDecisionIgnoresThePauseAndTheCaps(t *testing.T) {
	cfg := armCfg()
	cfg.Classes["docs-links"] = config.Class{Shadow: true}
	cfg.Merge.AutoMergesPerDay = 1
	d := DecideArm(ArmInputs{PR: agentPR(), Checks: green("SUCCESS"), Task: armTask("docs-links", "solo", ""), Cfg: cfg,
		ReportedHead: head, Paused: true, ArmedToday: 1, Files: changed("[a](https://x/old)", "[a](https://x/new)"), DiffRead: true})
	if d.Verdict != "shadow" || d.Reason != "shadow_would_arm" || !d.Paused || !d.Capped {
		t.Fatalf("%+v", d)
	}
	cfg.Classes["docs-links"] = config.Class{Live: true}
	if d := DecideArm(ArmInputs{PR: agentPR(), Checks: green("SUCCESS"), Task: armTask("docs-links", "solo", ""), Cfg: cfg,
		ReportedHead: head, Paused: true, Files: changed("[a](https://x/old)", "[a](https://x/new)"), DiffRead: true}); d.Reason != "class_paused" {
		t.Fatalf("a live class is paused: %+v", d)
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

// Human path AM1: with merge.approveToMerge, a maintainer's APPROVED review of the current head
// arms the merger; off, it hands off as before. Only the current head counts, the newest review of
// each maintainer counts, and a truncated list is a veto.
func TestAMaintainersApprovalArmsWhenTheSwitchIsOn(t *testing.T) {
	review := func(login, state, commit string, at int) forge.Review {
		return forge.Review{ID: int64(at), Author: login, State: state, Commit: commit, At: now.Add(time.Duration(at) * time.Minute)}
	}
	for name, c := range map[string]struct {
		on              bool
		reviews         []forge.Review
		truncated       bool
		verdict, reason string
	}{
		"off: an approval hands off as today":       {false, []forge.Review{review("Smana", "APPROVED", head, 1)}, false, "human", "maintainer_approved"},
		"on: an approval of the head arms":          {true, []forge.Review{review("Smana", "APPROVED", head, 1)}, false, "arm", "maintainer_approved"},
		"on: an approval of another head is stale":  {true, []forge.Review{review("Smana", "APPROVED", oldHead, 1)}, false, "human", "approval_stale"},
		"on: an approval with no commit is stale":   {true, []forge.Review{review("Smana", "APPROVED", "", 1)}, false, "human", "approval_stale"},
		"on: a newer request for changes vetoes":    {true, []forge.Review{review("Smana", "APPROVED", head, 1), review("Smana", "CHANGES_REQUESTED", head, 2)}, false, "human", "changes_requested"},
		"on: a later approval lifts the request":    {true, []forge.Review{review("Smana", "CHANGES_REQUESTED", head, 1), review("Smana", "APPROVED", head, 2)}, false, "arm", "maintainer_approved"},
		"on: a non-maintainer's approval is advice": {true, []forge.Review{review("alice", "APPROVED", head, 1)}, false, "shadow", "shadow_would_arm"},
		"on: a truncated review list vetoes":        {true, []forge.Review{review("Smana", "APPROVED", head, 1)}, true, "human", "changes_requested"},
		// R-B1: off, today's loop reads any maintainer approval, even one a newer request for changes follows.
		"off: an approval before a request for changes hands off as today": {false, []forge.Review{review("Smana", "APPROVED", head, 1), review("Smana", "CHANGES_REQUESTED", head, 2)}, false, "human", "maintainer_approved"},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := armCfg()
			cfg.Merge.ApproveToMerge = c.on
			cfg.Classes["docs-links"] = config.Class{Shadow: true}
			pr := agentPR()
			pr.Reviews, pr.ReviewsTruncated = c.reviews, c.truncated
			tk := armTask("docs-links", "solo", "")
			d := DecideArm(ArmInputs{PR: pr, Checks: green("SUCCESS"), Task: tk, Cfg: cfg, ReportedHead: head,
				Files: changed("[a](https://x/old)", "[a](https://x/new)"), DiffRead: true})
			if d.Verdict != c.verdict || d.Reason != c.reason {
				t.Fatalf("%s %s, want %s %s", d.Verdict, d.Reason, c.verdict, c.reason)
			}
			if d.Verdict == "arm" && d.Approver != "Smana" {
				t.Fatalf("approver %q", d.Approver)
			}
		})
	}
}

// AM1, "outside the caps": a maintainer's approval of the head arms even when the day's
// autoMergesPerDay or the class's maxPerDay is spent. The caps bound the factory's own judgement,
// not a human's.
func TestAnApprovalArmsOutsideTheCaps(t *testing.T) {
	for name, spent := range map[string]func(*ArmInputs){
		"the day's cap":   func(in *ArmInputs) { in.ArmedToday = in.Cfg.Merge.AutoMergesPerDay },
		"the class's cap": func(in *ArmInputs) { in.ClassArmedToday = 1 },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := armCfg()
			cfg.Merge.ApproveToMerge = true
			cfg.Classes["docs-links"] = config.Class{Live: true, MaxPerDay: 1}
			pr := agentPR()
			pr.Reviews = []forge.Review{{ID: 1, Author: "Smana", State: "APPROVED", Commit: head, At: now}}
			in := ArmInputs{PR: pr, Checks: green("SUCCESS"), Task: armTask("docs-links", "solo", ""), Cfg: cfg, ReportedHead: head,
				Files: changed("[a](https://x/old)", "[a](https://x/new)"), DiffRead: true}
			spent(&in)
			if d := DecideArm(in); d.Verdict != "arm" || d.Reason != "maintainer_approved" {
				t.Fatalf("%+v, want arm maintainer_approved", d)
			}
			in.PR.Reviews = nil
			if d := DecideArm(in); d.Reason != "auto_merge_cap" {
				t.Fatalf("without the approval: %+v, want auto_merge_cap", d)
			}
		})
	}
}

// R-B1 (AM1, "adopted classes keep today's path"): with the switch on, a maintainer's approval of
// an adopted pull request's head still hands off; it never arms the merger.
func TestAnAdoptedPullRequestsApprovalHandsOffWithTheSwitchOn(t *testing.T) {
	cfg := armCfg()
	cfg.Merge.ApproveToMerge = true
	cfg.Classes["deps-patch"] = depsPatchClass()
	pr := forge.PR{Author: bot, HeadRef: "renovate/kyverno-3.x", HeadSHA: head,
		Reviews: []forge.Review{{ID: 1, Author: "Smana", State: "APPROVED", Commit: head, At: now}}}
	d := DecideArm(ArmInputs{PR: pr, Checks: green("SUCCESS"), Task: armTask("deps-patch", "solo", ""), Cfg: cfg})
	if d.Verdict != "human" || d.Reason != "maintainer_approved" || d.Approver != "" {
		t.Fatalf("%+v, want human maintainer_approved", d)
	}
}
