// SPDX-License-Identifier: Apache-2.0

package summary

import "fmt"

// Step kinds: where a human acts on a room that waits for them.
const (
	StepApprove = "approve" // the room page: gates and high-cost runs are decided there
	StepPR      = "pr"      // GitHub: merge or close the pull request; nothing is done in the room
	StepAnswer  = "answer"  // GitHub: the issue's question is answered on the issue
)

// Step is the one thing a human does for a waiting room, and where. It is the same sentence in the
// room page, roomctl and the MCP tools.
type Step struct {
	Kind  string `json:"kind"`
	Label string `json:"label"`
	URL   string `json:"url,omitempty"`
}

// StepIn is what StepFor reads: the task's facts as the room row or the summary holds them, and
// what the caller may do. A sealed room has no step: leave it out (Sealed).
type StepIn struct {
	Sealed    bool
	Phase     string
	Reason    string
	PR        int
	PRURL     string
	Issue     int
	IssueURL  string
	Approvals bool // the caller may decide and one is live
	CanClose  bool // the caller may close the room's task (policy.Close): merge or answer need it
	RoomURL   string
}

// StepFor is the step of a room, nil when nothing waits for this caller. A live approval comes
// first: it is decided on the room page. Else a task in AwaitingHuman with a pull request waits
// for a merge or a close on GitHub, whatever the reason (shadow_would_arm, policy_pending,
// class_unarmed, maintainer_approved, changes_requested, ...), and an Escalated needs_author task
// waits for an answer on its issue: both only for a caller who may close, as Merge and Answer.
func StepFor(in StepIn) *Step {
	switch {
	case in.Sealed:
		return nil
	case in.Approvals:
		return &Step{Kind: StepApprove, Label: "approve here", URL: in.RoomURL}
	case in.CanClose && in.Phase == "AwaitingHuman" && in.PR > 0:
		return &Step{Kind: StepPR, Label: fmt.Sprintf("review and merge or close PR #%d", in.PR), URL: in.PRURL}
	case in.CanClose && in.Phase == "Escalated" && in.Reason == "needs_author" && in.Issue > 0:
		return &Step{Kind: StepAnswer, Label: fmt.Sprintf("answer the question on issue #%d", in.Issue), URL: in.IssueURL}
	}
	return nil
}
