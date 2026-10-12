// SPDX-License-Identifier: Apache-2.0

package summary

import (
	"fmt"
	"regexp"

	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/policy"
)

// maxReason bounds a reason shown as the factory wrote it, in runes.
const maxReason = 200

// repoRE reads owner/name out of a validated github.com issue or pull request URL
// (envelope.TaskFacts.Validate).
var repoRE = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9-]+/[A-Za-z0-9._-]+)/(?:issues|pull)/[0-9]+$`)

// repoOf is the repository a task's issue or pull request lives in, "" when the facts name neither.
// Every generated gh command passes it as --repo: run from another checkout, the command would
// otherwise act on that checkout's issue of the same number (#59).
func repoOf(f *envelope.TaskFacts) string {
	if f == nil {
		return ""
	}
	for _, u := range []string{urlOf(f.PR), urlOfIssue(f.Issue)} {
		if m := repoRE.FindStringSubmatch(u); m != nil {
			return m[1]
		}
	}
	return ""
}

func urlOf(p *envelope.PRFact) string {
	if p == nil {
		return ""
	}
	return p.URL
}

func urlOfIssue(i *envelope.IssueFact) string {
	if i == nil {
		return ""
	}
	return i.URL
}

// repoFlag is " --repo owner/name", or nothing when the repository is unknown.
func repoFlag(repo string) string {
	if repo == "" {
		return ""
	}
	return " --repo " + repo
}

// MergeHow is how a maintainer merges pull request n under the repository ruleset
// (agent-branches restricts updates on every branch but agent/**, so an approval alone leaves
// "Merging is blocked"): tick the bypass box, or merge as an admin (#51).
func MergeHow(repo string, n int) string {
	return fmt.Sprintf("merge PR #%d: the ruleset blocks a plain merge, so tick \"bypass rules\" on the PR page, or run `gh pr merge %d --squash --admin%s`",
		n, n, repoFlag(repo))
}

// Merge is the need of a task that waits for a maintainer's merge: AwaitingHuman with a pull
// request. Whoever owns the room, the maintainer, sees it; GitHub still decides who may merge.
func Merge(f *envelope.TaskFacts, sub policy.Subject) (Need, bool) {
	if f == nil || f.Phase != "AwaitingHuman" || f.PR == nil || f.PR.Number <= 0 || !policy.Allowed(sub, policy.Close) {
		return Need{}, false
	}
	return Need{Kind: "merge", ID: fmt.Sprint(f.PR.Number), What: MergeHow(repoOf(f), f.PR.Number), URL: f.PR.URL}, true
}

// reasons is the prose of the wait reasons a task shows, repeated from the factory's narration
// (internal/factory/narrate) like stopLabel: this package stays free of the factory's imports.
func reasons() map[string]string {
	return map[string]string{
		"waiting_run_slot":          "the factory runs its tasks one at a time and another one has the slot",
		"waiting_daily_budget":      "the factory's daily token budget is spent",
		"waiting_room_log":          "the room's log is not open on the broker yet",
		"waiting_broker_permission": "the broker does not let the factory write to rooms yet",
		"ci_pending":                "CI has not finished",
		"merge_gate_off":            "no merge class is live, so the factory does not gate this pull request",
		"policy_pending":            "the policy needs a maintainer's approval",
		"policy_absent":             "policy-bot has not evaluated the pull request",
		"maintainer_approved":       "a maintainer approved it",
		"not_mergeable":             "GitHub does not allow merging it as it stands",
		"gate_path":                 "it touches a gate path, so an agent can never merge it",
		"verdict_stale":             "the reviewer's approval was not for the pull request's current head",
		"human_commit_in_pr":        "it holds a commit no run of the task reported",
		"shadow_would_arm":          "it would auto-merge, and the merge gate is in shadow",
		"auto_merge_cap":            "today's auto-merge cap is reached",
		"stuck":                     "the run showed no activity for 10 minutes",
		"ci_red":                    "CI stayed red after the fix runs",
		"secret_scan_red":           "the secret scan found a live credential in the pull request",
		"review_rounds_exhausted":   "the reviewer still asked for changes after the last review round",
		"no_verdict":                "the last review ended without a verdict the factory can act on",
		"budget-task":               "the task's token budget has no room left for another run",
		"resume_budget":             "the sandbox was lost and the task's token budget cannot cover another run",
		"run_unschedulable":         "the cluster never admitted the task's run in time",
		"pod_lost":                  "the sandbox was lost (spot reclaim or eviction)",
		"deadline":                  "the run hit its wall-clock limit",
		"agent_error":               "the agent stopped on an error",
		"agent_stuck":               "the agent reported that it was stuck",
		"no_pr":                     "the agent opened no pull request",
	}
}

const escalatedNext = "a maintainer comments `/factory retry` on the issue to run it again, or pushes to the branch"

// nextSteps are the specific next steps; waitingDefault falls back to the phase's.
func nextSteps() map[string]string {
	return map[string]string{
		"waiting_daily_budget": "nothing to do: it resumes after 00:00 UTC",
		"secret_scan_red":      "a maintainer revokes the credential and closes the pull request; the factory does not retry",
		"ci_pending":           "nothing to do: the task goes on when CI finishes",
		"verdict_stale":        "a maintainer reviews the current head, or the factory asks for a new review",
	}
}

// waiting is the reason and next step of a task that waits on something other than its own run;
// both are "" while it works or once it ended in a way that needs no one. A reason the map does
// not know is shown as written, bounded: it is the factory's own string, never an agent's.
func waiting(f *envelope.TaskFacts) (reason, next string) {
	if f == nil {
		return "", ""
	}
	why := reasons()[f.Reason]
	if why == "" {
		why = holds()[f.Reason]
	}
	if why == "" && f.Reason != "" {
		why = f.Reason
		if r := []rune(why); len(r) > maxReason {
			why = string(r[:maxReason]) + "…"
		}
	}
	next = nextSteps()[f.Reason]
	switch f.Phase {
	case "Queued":
		if why == "" {
			why = "queued for a run"
		}
		if next == "" {
			next = "nothing to do: the next run starts once the room and the factory are free"
			if _, ok := holds()[f.Reason]; ok {
				next = "hand the driver back or end the other run: the task's next run then starts"
			}
		}
	case "AwaitingCI":
		if why == "" {
			why = reasons()["ci_pending"]
		}
		if next == "" {
			next = "nothing to do: the task goes on when CI finishes"
		}
	case "AwaitingHuman":
		if why == "" {
			why = "the pull request is ready for a maintainer"
		}
		if next == "" {
			next = "a maintainer reviews and merges the pull request"
			if f.PR == nil {
				next = "a maintainer looks at the task's issue"
			}
		}
	case "Escalated":
		if why == "" {
			why = "the factory could not finish the task"
		}
		if next == "" {
			next = escalatedNext
		}
	default:
		return "", ""
	}
	return why, next
}
