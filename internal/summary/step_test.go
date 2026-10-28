// SPDX-License-Identifier: Apache-2.0

package summary

import "testing"

func TestStepFor(t *testing.T) {
	const pr, room = "https://github.com/o/r/pull/7", "https://rooms.example/r/room-1"
	cases := []struct {
		name      string
		phase     string
		pr        int
		approvals bool
		kind      string
	}{
		{"approval wins over a held PR", "AwaitingHuman", 7, true, StepApprove},
		{"approval alone", "Implementing", 0, true, StepApprove},
		{"held PR", "AwaitingHuman", 7, false, StepPR},
		{"awaiting human, no PR", "AwaitingHuman", 0, false, ""},
		{"CI wait", "AwaitingCI", 7, false, ""},
		{"queued", "Queued", 0, false, ""},
	}
	for _, c := range cases {
		s := StepFor(c.phase, c.pr, pr, c.approvals, room)
		switch {
		case c.kind == "" && s != nil:
			t.Errorf("%s: got %+v, want none", c.name, s)
		case c.kind != "" && (s == nil || s.Kind != c.kind):
			t.Errorf("%s: got %+v, want kind %s", c.name, s, c.kind)
		}
	}
	if s := StepFor("AwaitingHuman", 7, pr, false, room); s.Label != "review and merge or close PR #7" || s.URL != pr {
		t.Errorf("pr step = %+v", s)
	}
	if s := StepFor("AwaitingHuman", 7, pr, true, room); s.Label != "approve here" || s.URL != room {
		t.Errorf("approve step = %+v", s)
	}
}
