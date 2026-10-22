// SPDX-License-Identifier: Apache-2.0

package envelope

import "slices"

// HarnessEnd is the state_changed kind a bridge records when agent-run reports why the loop
// policy ended its run (loops design, "How a run ends"): {"kind":"harness_end","reason":…}.
const HarnessEnd = "harness_end"

// The loop policy's end reasons, as agent-run sends them on its final read (harness R13).
const (
	EndJudgeImpossible = "judge_impossible"
	EndStalled         = "stalled"
	EndNotMet          = "not_met"
)

// LoopEnd reports whether reason is one of the loop policy's end reasons.
func LoopEnd(reason string) bool {
	return slices.Contains([]string{EndJudgeImpossible, EndStalled, EndNotMet}, reason)
}
