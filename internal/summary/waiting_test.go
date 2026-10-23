// SPDX-License-Identifier: Apache-2.0

package summary_test

import (
	"strings"
	"testing"
	"time"

	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/policy"
	"github.com/Smana/agent-platform/internal/summary"
)

const (
	issueURL = "https://github.com/Smana/cloud-native-ref/issues/2266"
	prURL    = "https://github.com/Smana/cloud-native-ref/pull/2267"
)

func taskView(f envelope.TaskFacts, sub policy.Subject) summary.Summary {
	factory := envelope.Actor{Kind: envelope.ActorSystem, ID: "system:factory"}
	st := summary.Fold([]envelope.Event{ev(1, envelope.StateChanged, factory, envelope.TaskStatePayload(f))})
	return summary.View(st, "r", "https://rooms.example/r/r", sub, 0, time.Now())
}

// #59: a generated command run from another checkout would act on that checkout's repository.
func TestEveryGeneratedGhCommandNamesItsRepository(t *testing.T) {
	s := taskView(envelope.TaskFacts{Phase: "Implementing", Issue: &envelope.IssueFact{Number: 2266, URL: issueURL}}, owner())
	var seen int
	for _, a := range s.Actions {
		if strings.Contains(a.CLI, "gh ") {
			seen++
			if !strings.Contains(a.CLI, "--repo Smana/cloud-native-ref") {
				t.Fatalf("no --repo: %q", a.CLI)
			}
		}
	}
	if seen == 0 {
		t.Fatal("the owner is offered no gh command to check")
	}
}

// #59: every phase the task waits in says why and what comes next.
func TestEveryWaitingPhaseHasAReasonAndANextStep(t *testing.T) {
	waits := []envelope.TaskFacts{
		{Phase: "Queued"}, {Phase: "Queued", Reason: "waiting_run_slot"}, {Phase: "Queued", Reason: "waiting_daily_budget"},
		{Phase: "Queued", Reason: "waiting_human_driver"}, {Phase: "AwaitingCI"}, {Phase: "AwaitingCI", Reason: "ci_pending"}, {Phase: "AwaitingCI", Reason: "holdout_pending"},
		{Phase: "AwaitingHuman"}, {Phase: "AwaitingHuman", Reason: "policy_pending"}, {Phase: "AwaitingHuman", Reason: "not_mergeable"},
		{Phase: "Escalated", Reason: "stuck"}, {Phase: "Escalated", Reason: "ci_red"}, {Phase: "Escalated", Reason: "never_heard_of_it"},
	}
	for _, f := range waits {
		s := taskView(f, watcher())
		if s.Status.Reason == "" || s.Status.Next == "" {
			t.Errorf("%s/%s: reason %q, next %q", f.Phase, f.Reason, s.Status.Reason, s.Status.Next)
		}
	}
	if s := taskView(envelope.TaskFacts{Phase: "AwaitingCI", Reason: "holdout_pending"}, watcher()); !strings.Contains(s.Status.Next, "holdout checks report") {
		t.Errorf("holdout_pending has its own next step: %q", s.Status.Next)
	}
	if s := taskView(envelope.TaskFacts{Phase: "Implementing"}, watcher()); s.Status.Reason != "" || s.Status.Next != "" {
		t.Errorf("a running task is not waiting: %+v", s.Status)
	}
	long := taskView(envelope.TaskFacts{Phase: "Escalated", Reason: strings.Repeat("x", 5000)}, watcher())
	if len([]rune(long.Status.Reason)) > 300 {
		t.Errorf("the reason is unbounded: %d runes", len([]rune(long.Status.Reason)))
	}
}

// #51: a PR waiting on the maintainer's merge is a need, with how to merge under the ruleset.
func TestAwaitingHumanOnAMergeNeedsTheMaintainerAndSaysHow(t *testing.T) {
	f := envelope.TaskFacts{Phase: "AwaitingHuman", Issue: &envelope.IssueFact{Number: 2266, URL: issueURL},
		PR: &envelope.PRFact{Number: 2267, URL: prURL}}
	got := taskView(f, owner()).NeedsYou
	if len(got) != 1 || got[0].Kind != "merge" || got[0].URL != prURL ||
		!strings.Contains(got[0].What, "#2267") || !strings.Contains(got[0].What, "bypass") ||
		!strings.Contains(got[0].What, "gh pr merge 2267 --squash --admin --repo Smana/cloud-native-ref") {
		t.Fatalf("needs you: %+v", got)
	}
	if got := taskView(f, watcher()).NeedsYou; len(got) != 0 {
		t.Fatalf("a watcher cannot merge: %+v", got)
	}
	f.PR = nil
	if got := taskView(f, owner()).NeedsYou; len(got) != 0 {
		t.Fatalf("no PR, nothing to merge: %+v", got)
	}
	f.PR = &envelope.PRFact{Number: 2267, URL: prURL}
	f.Phase = "Reviewing"
	if got := taskView(f, owner()).NeedsYou; len(got) != 0 {
		t.Fatalf("not waiting yet: %+v", got)
	}
}

// Only a repository admin can bypass the ruleset: the text says who, not "you".
func TestTheMergeNeedNamesWhoCanBypass(t *testing.T) {
	f := envelope.TaskFacts{Phase: "AwaitingHuman", PR: &envelope.PRFact{Number: 2267, URL: prURL}}
	got := taskView(f, owner()).NeedsYou
	if len(got) != 1 || !strings.Contains(got[0].What, "a maintainer with admin on Smana/cloud-native-ref merges PR #2267") {
		t.Fatalf("%+v", got)
	}
}

// No repository to name: no gh command at all, rather than one that acts on the wrong checkout.
func TestNoRepositoryNoGhCommand(t *testing.T) {
	f := envelope.TaskFacts{Phase: "AwaitingHuman", Issue: &envelope.IssueFact{Number: 2266}, PR: &envelope.PRFact{Number: 2267}}
	s := taskView(f, owner())
	for _, a := range s.Actions {
		if a.Kind == "stop" || strings.Contains(a.CLI, "gh ") {
			t.Fatalf("a gh command with no repository: %+v", a)
		}
	}
	if len(s.NeedsYou) != 1 || strings.Contains(s.NeedsYou[0].What, "gh ") || !strings.Contains(s.NeedsYou[0].What, "bypass") {
		t.Fatalf("%+v", s.NeedsYou)
	}
}

func TestTheIssueTitleIsCarried(t *testing.T) {
	s := taskView(envelope.TaskFacts{Phase: "Implementing", Issue: &envelope.IssueFact{Number: 1, URL: issueURL, Title: "docs: fix it"}}, watcher())
	if s.Status.Issue.Title != "docs: fix it" {
		t.Fatalf("%+v", s.Status.Issue)
	}
}
