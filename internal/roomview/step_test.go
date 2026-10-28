// SPDX-License-Identifier: Apache-2.0

package roomview

import (
	"strings"
	"testing"

	"github.com/Smana/agent-platform/internal/summary"
)

const prLink = "https://github.com/Smana/cloud-native-ref/pull/2239"

func prStep() *summary.Step {
	return summary.StepFor(summary.StepIn{Phase: "AwaitingHuman", PR: 2239, PRURL: prLink, CanClose: true, RoomURL: rooms + "/r/x"})
}

// A room held on a PR names the action and links the PR, with or without a need (a caller who may
// not close has none), in both formats.
func TestOverviewNamesThePRStep(t *testing.T) {
	for _, reason := range []string{"shadow_would_arm", "policy_pending", "class_unarmed", "maintainer_approved", "changes_requested"} {
		for _, need := range []*summary.Need{nil, {Kind: "merge", ID: "2239", URL: prLink}} {
			row := Row{ID: "x", URL: rooms + "/r/x", State: summary.StateWaiting, Repository: "Smana/cloud-native-ref", Issue: 1, Title: "t",
				Task: "AwaitingHuman", PR: 2239, Reason: reason, Need: need, Step: prStep(), LastEventAt: now}
			md := render(t, []Row{row}, Options{Format: Markdown, Now: now, Rooms: rooms})
			if !strings.Contains(md, "[review and merge or close PR \\#2239]("+prLink+")") {
				t.Errorf("%s markdown lacks the linked step:\n%s", reason, md)
			}
			txt := render(t, []Row{row}, Options{Format: Text, Now: now, Rooms: rooms})
			if !strings.Contains(txt, "review and merge or close PR #2239 ("+prLink+")") {
				t.Errorf("%s text lacks the linked step:\n%s", reason, txt)
			}
		}
	}
}

func TestOverviewApprovalKeepsItsCell(t *testing.T) {
	row := Row{ID: "x", URL: rooms + "/r/x", State: summary.StateWaiting, Task: "AwaitingHuman", NeedsYou: 1, LastEventAt: now,
		Need: &summary.Need{Kind: "approval", ID: "a", What: "git push", URL: rooms + "/r/x#a"},
		Step: summary.StepFor(summary.StepIn{Phase: "AwaitingHuman", PR: 3, PRURL: prLink, Approvals: true, CanClose: true, RoomURL: rooms + "/r/x"})}
	if out := render(t, []Row{row}, Options{Format: Text, Now: now, Rooms: rooms}); !strings.Contains(out, `approve "git push"`) || strings.Contains(out, "PR #") {
		t.Errorf("approval cell:\n%s", out)
	}
}

func TestCardNamesTheStep(t *testing.T) {
	var s summary.Summary
	s.Room, s.URL = "x", rooms+"/r/x"
	s.Status.Phase = "AwaitingHuman"
	s.Step = prStep()
	md := card1(t, s, Options{Format: Markdown, Now: now, Rooms: rooms})
	if !strings.Contains(md, "**Your move:** [review and merge or close PR #2239]("+prLink+")") {
		t.Errorf("markdown card:\n%s", md)
	}
	txt := card1(t, s, Options{Format: Text, Now: now, Rooms: rooms})
	if !strings.Contains(txt, "Your move: review and merge or close PR #2239 ("+prLink+")") {
		t.Errorf("text card:\n%s", txt)
	}
	s.Step = summary.StepFor(summary.StepIn{Phase: "Implementing", Approvals: true, RoomURL: s.URL})
	if txt = card1(t, s, Options{Format: Text, Now: now, Rooms: rooms}); !strings.Contains(txt, "Your move: approve here ("+rooms+"/r/x)") {
		t.Errorf("approve card:\n%s", txt)
	}
}

func TestOverviewNamesTheAnswerStep(t *testing.T) {
	const issue = "https://github.com/Smana/cloud-native-ref/issues/9"
	row := Row{ID: "x", URL: rooms + "/r/x", State: summary.StateWaiting, Task: "Escalated", Reason: "needs_author", LastEventAt: now,
		Need: &summary.Need{Kind: "answer", ID: "9", URL: issue},
		Step: summary.StepFor(summary.StepIn{Phase: "Escalated", Reason: "needs_author", Issue: 9, IssueURL: issue, CanClose: true})}
	if out := render(t, []Row{row}, Options{Format: Markdown, Now: now, Rooms: rooms}); !strings.Contains(out, "[answer the question on issue \\#9]("+issue+")") {
		t.Errorf("markdown:\n%s", out)
	}
}
