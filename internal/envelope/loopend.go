// SPDX-License-Identifier: Apache-2.0

package envelope

import "slices"

// HarnessEnd is the state_changed kind a bridge records when agent-run reports why the loop
// policy, or the scoper's tool-call limit, ended its run (loops design, "How a run ends"):
// {"kind":"harness_end","reason":…}.
const HarnessEnd = "harness_end"

// The loop policy's end reasons, as agent-run sends them on its final read (harness R13).
const (
	EndJudgeImpossible = "judge_impossible"
	EndStalled         = "stalled"
	EndNotMet          = "not_met"
)

// EndBudgetCalls is the harness's end of a scoper run stopped at its tool-call limit before any
// handoff (harness roles HR2). The factory fails it open, as a missing scope.
const EndBudgetCalls = "budget_calls"

// LoopEnd reports whether reason is one of the harness's end reasons: the loop policy's three and
// the scoper's tool-call limit.
func LoopEnd(reason string) bool {
	return slices.Contains([]string{EndJudgeImpossible, EndStalled, EndNotMet, EndBudgetCalls}, reason)
}
