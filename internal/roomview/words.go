// SPDX-License-Identifier: Apache-2.0

package roomview

import "github.com/Smana/maku/internal/summary"

// word is the phrase a vocabulary has for v, else v cleaned: a value this broker does not know is
// still untrusted text.
func word(m map[string]string, v string) string {
	if w, ok := m[v]; ok {
		return w
	}
	return clean(v)
}

// stateWord is a coarse state in words. A room that needs the caller reads "needs you" whatever
// its state says, and only then: the broker decides, never the agent.
func stateWord(state string, needsYou bool) string {
	if needsYou {
		return "needs you"
	}
	return word(map[string]string{
		summary.StateWaiting: "waiting for a human", summary.StateRunning: "running", summary.StateQueued: "queued",
		summary.StateDone: "done", summary.StateFailed: "failed", summary.StateStopped: "stopped",
	}, state)
}

var phases = map[string]string{
	"Received": "received", "Triaged": "triaged", "Queued": "queued for a run", "Split": "split into parts",
	"Implementing": "an agent is coding", "Reviewing": "an agent is reviewing", "AwaitingCI": "waiting for CI",
	"AutoMerging": "auto-merging", "Merged": "merged, verifying", "Verifying": "verifying the merge",
	"AwaitingHuman": "waiting for a human", "Escalated": "handed to a maintainer",
	"Done": "done", "NoOp": "nothing to change", "Rejected": "rejected", "Reverted": "merge reverted",
	"Stopped": "stopped", "Closed": "closed",
	// A room's own phases, for a room no task owns.
	"Open": "open", "Active": "an agent is working", "Idle": "idle", "Sealed": "sealed, its log is full",
}

// phaseWords is a Task phase in words.
func phaseWords(phase string) string { return word(phases, phase) }

var reasonPhrases = map[string]string{
	"adopt_ref_invalid":         "the adopted branch name is invalid",
	"adopted_head_moved":        "the adopted branch moved",
	"adoption_unread":           "the adopted pull request is unreadable",
	"agent_error":               "the agent stopped on an error",
	"agent_stuck":               "the agent said it was stuck",
	"auto_merge_cap":            "today's auto-merge cap is reached",
	"branch_not_allowed":        "the branch is not allowed",
	"budget-family":             "the task family is out of budget",
	"budget-task":               "the task is out of budget",
	"changes_requested":         "changes were requested",
	"child_dropped":             "a part was dropped",
	"children_merged":           "every part merged",
	"ci_cancelled":              "a check was cancelled",
	"ci_pending":                "CI is running",
	"ci_red":                    "CI is red",
	"ci_wait_expired":           "CI took too long",
	"closes_parent":             "it would close the parent issue",
	"deadline":                  "the run hit its time limit",
	"family_dropped":            "the split was dropped",
	"fork_pull_request":         "the pull request is from a fork",
	"gate_path":                 "it touches a gate path",
	"holdout_pending":           "waiting for holdout checks",
	"human_commit_in_pr":        "a human commit is in the pull request",
	"judge_impossible":          "the judge found it cannot be done",
	"ledger_truncated":          "the merge ledger read is incomplete",
	"loop_unsupported":          "the cluster lacks loop support",
	"maintainer_approved":       "a maintainer approved it",
	"approval_stale":            "the approval is of an older head",
	"reentered_approved":        "approved, decided again",
	"reentered_day_turned":      "a new day, decided again",
	"reentered_unpaused":        "the pause lifted, decided again",
	"merge_gate_off":            "the merge gate is off",
	"needs_author":              "the issue's author must answer",
	"no_pr":                     "the agent opened no pull request",
	"no_verdict":                "the review gave no verdict",
	"not_adopted_author":        "the pull request's author is not adopted",
	"not_mergeable":             "the pull request is not mergeable",
	"not_met":                   "the acceptance checks are unmet",
	"parent_missing":            "the parent task is gone",
	"parent_not_split":          "the parent is no longer waiting",
	"pod_lost":                  "the sandbox was lost",
	"policy_absent":             "no merge policy applies",
	"policy_pending":            "the merge policy is not ready",
	"push_unrecorded":           "GitHub has not recorded the push",
	"release_age_pending":       "the release is too new",
	"release_age_red":           "the release age check failed",
	"resume_budget":             "no budget to resume the task",
	"review_rounds_exhausted":   "the reviewer still wants changes",
	"run_unschedulable":         "the cluster never admitted the run",
	"secret_scan_red":           "the secret scan found a credential",
	"shadow_would_arm":          "it would merge, the gate is in shadow",
	"split_proposed":            "a split is proposed",
	"contract_invalid":          "the tester's branch is not valid",
	"contract_disputed":         "the tests are disputed",
	"contract_too_large":        "the contract is too big to record",
	"contract_check_unset":      "the contract has no check to prove it",
	"contract_modified":         "the pull request changes the tests",
	"contract_unread":           "reading the changes to the tests",
	"contract_push_unrecorded":  "waiting for GitHub to see the tests",
	"stalled":                   "the run made no commit",
	"class_unarmed":             "a maintainer merges this class",
	"steering_path":             "it changes a steering note",
	"stuck":                     "the run showed no activity",
	"verdict_stale":             "the approval is for an older head",
	"waiting_active_tasks":      "waiting for other tasks to finish",
	"waiting_broker_permission": "waiting for the broker's permission",
	"waiting_daily_budget":      "waiting for tomorrow's budget",
	"waiting_review_wip":        "waiting for a review slot",
	"waiting_room_log":          "waiting for the room's log",
	"waiting_run_slot":          "waiting for a run slot",
}

// reasonWords is a wait reason as a short phrase, at most 40 runes: summary's sentence for it is
// longer than a table cell wants.
func reasonWords(reason string) string { return word(reasonPhrases, reason) }

// triggerWords is why a run exists. A human trigger reads "human review", as it is a person's
// review request that starts it.
func triggerWords(trigger string) string {
	return word(map[string]string{
		"initial": "first run", "review": "review", "human": "human review", "ci": "CI fix", "retry": "retry", "resume": "resume",
	}, trigger)
}

// outcomeWords is how a run ended; a run that has not ended is running.
func outcomeWords(outcome string) string {
	if outcome == "" {
		return "running"
	}
	return word(map[string]string{
		"succeeded": "succeeded", "failed": "failed", "budget_exhausted": "out of budget",
		"revoked": "revoked", "lost": "lost", "ended": "ended",
	}, outcome)
}

// verdictWords is a reviewer's verdict in words.
// judgeWords is the completion judge's verdict as words.
func judgeWords(verdict string) string {
	return word(map[string]string{"met": "met", "not_yet": "not yet", "impossible": "impossible"}, verdict)
}

func verdictWords(verdict string) string {
	return word(map[string]string{
		"approve": "approved", "approved": "approved", "changes": "changes requested", "none": "no verdict",
	}, verdict)
}
