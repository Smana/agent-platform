// SPDX-License-Identifier: Apache-2.0

package summary

import (
	"testing"
	"time"

	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/policy"
)

func TestStepFor(t *testing.T) {
	const pr, issue, room = "https://github.com/o/r/pull/7", "https://github.com/o/r/issues/3", "https://rooms.example/r/room-1"
	held := StepIn{Phase: "AwaitingHuman", PR: 7, PRURL: pr, CanClose: true, RoomURL: room}
	with := func(f func(*StepIn)) StepIn { in := held; f(&in); return in }
	cases := []struct {
		name  string
		in    StepIn
		kind  string
		label string
		url   string
	}{
		{"approval wins over a held PR", with(func(i *StepIn) { i.Approvals = true }), StepApprove, "approve here", room},
		{"approval alone", StepIn{Phase: "Implementing", Approvals: true, RoomURL: room}, StepApprove, "approve here", room},
		{"held PR", held, StepPR, "review and merge or close PR #7", pr},
		{"held PR, caller may not close", with(func(i *StepIn) { i.CanClose = false }), "", "", ""},
		{"awaiting human, no PR", with(func(i *StepIn) { i.PR = 0 }), "", "", ""},
		{"CI wait", with(func(i *StepIn) { i.Phase = "AwaitingCI" }), "", "", ""},
		{"queued", StepIn{Phase: "Queued", CanClose: true}, "", "", ""},
		{"sealed room", with(func(i *StepIn) { i.Sealed = true; i.Approvals = true }), "", "", ""},
		{"needs author", StepIn{Phase: "Escalated", Reason: "needs_author", Issue: 3, IssueURL: issue, CanClose: true}, StepAnswer, "answer the question on issue #3", issue},
		{"needs author, caller may not close", StepIn{Phase: "Escalated", Reason: "needs_author", Issue: 3, IssueURL: issue}, "", "", ""},
		{"escalated for another reason", StepIn{Phase: "Escalated", Reason: "stuck", Issue: 3, CanClose: true}, "", "", ""},
	}
	for _, c := range cases {
		s := StepFor(c.in)
		switch {
		case c.kind == "" && s != nil:
			t.Errorf("%s: got %+v, want none", c.name, s)
		case c.kind != "" && (s == nil || *s != Step{Kind: c.kind, Label: c.label, URL: c.url}):
			t.Errorf("%s: got %+v", c.name, s)
		}
	}
}

func TestViewStepFollowsTheCaller(t *testing.T) {
	pr := &envelope.PRFact{Number: 7, URL: "https://github.com/o/r/pull/7"}
	issue := &envelope.IssueFact{Number: 3, URL: "https://github.com/o/r/issues/3"}
	owner := policy.Subject{Kind: envelope.ActorHuman, Role: policy.Owner, WebUI: true}
	watcher := policy.Subject{Kind: envelope.ActorHuman, Role: policy.Watcher, WebUI: true}
	now := time.Now()
	for _, tc := range []struct {
		name string
		st   State
		sub  policy.Subject
		kind string
	}{
		{"merge for an owner", State{Facts: &envelope.TaskFacts{Phase: "AwaitingHuman", PR: pr}}, owner, StepPR},
		{"no merge for a watcher", State{Facts: &envelope.TaskFacts{Phase: "AwaitingHuman", PR: pr}}, watcher, ""},
		{"answer for an owner", State{Facts: &envelope.TaskFacts{Phase: "Escalated", Reason: "needs_author", Issue: issue}}, owner, StepAnswer},
		{"no answer for a watcher", State{Facts: &envelope.TaskFacts{Phase: "Escalated", Reason: "needs_author", Issue: issue}}, watcher, ""},
		{"sealed room", State{Sealed: "Closed", Facts: &envelope.TaskFacts{Phase: "AwaitingHuman", PR: pr}}, owner, ""},
	} {
		got := View(tc.st, "r", "https://rooms.example/r/r", tc.sub, 0, now).Step
		if (got == nil) != (tc.kind == "") || got != nil && got.Kind != tc.kind {
			t.Errorf("%s: step = %+v, want %q", tc.name, got, tc.kind)
		}
	}
}
