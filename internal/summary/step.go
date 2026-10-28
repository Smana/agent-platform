// SPDX-License-Identifier: Apache-2.0

package summary

import "fmt"

// Step kinds: where a human acts on a room that waits for them.
const (
	StepApprove = "approve" // the room page: gates and high-cost runs are decided there
	StepPR      = "pr"      // GitHub: merge or close the pull request; nothing is done in the room
)

// Step is the one thing a human does for a waiting room, and where. It is the same sentence in the
// room page, roomctl and the MCP tools.
type Step struct {
	Kind  string `json:"kind"`
	Label string `json:"label"`
	URL   string `json:"url,omitempty"`
}

// StepFor is the step of a room, nil when nothing waits for a human. A live approval comes first
// (approvals: the caller may decide one): it is decided on the room page, roomURL. Else a task in
// AwaitingHuman with a pull request waits for a merge or a close on GitHub, whatever the reason
// (shadow_would_arm, policy_pending, class_unarmed, maintainer_approved, changes_requested, ...):
// the room has nothing to approve then.
func StepFor(phase string, pr int, prURL string, approvals bool, roomURL string) *Step {
	switch {
	case approvals:
		return &Step{Kind: StepApprove, Label: "approve here", URL: roomURL}
	case phase == "AwaitingHuman" && pr > 0:
		return &Step{Kind: StepPR, Label: fmt.Sprintf("review and merge or close PR #%d", pr), URL: prURL}
	}
	return nil
}
