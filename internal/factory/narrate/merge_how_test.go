// SPDX-License-Identifier: Apache-2.0

package narrate

import (
	"strings"
	"testing"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/config"
	summ "github.com/Smana/agent-platform/internal/summary"
)

// waitReasons are the reasons a task waits or hands off with (merge gate, run end, queue holds):
// the summary shows each as prose, never the raw key.
var waitReasons = []string{
	"ci_pending", "merge_gate_off", "policy_pending", "policy_absent", "not_mergeable", "gate_path", "steering_path", "verdict_stale",
	"human_commit_in_pr", "shadow_would_arm", "auto_merge_cap", "stuck", "ci_red", "secret_scan_red", "review_rounds_exhausted",
	"no_verdict", "budget-task", "resume_budget", "run_unschedulable", "pod_lost", "deadline", "agent_error", "agent_stuck",
	"no_pr", "waiting_human_driver", "waiting_room_busy", "waiting_run_slot", "waiting_active_tasks", "waiting_review_wip",
	"release_age_pending", "release_age_red", "push_unrecorded", "changes_requested", "fork_pull_request",
	"not_adopted_author", "branch_not_allowed", "adoption_unread", "ci_cancelled", "adopted_head_moved", "adopt_ref_invalid",
	"contract_invalid", "contract_disputed", "contract_modified", "contract_unread", "contract_push_unrecorded",
	"waiting_daily_budget", "waiting_room_log", "waiting_broker_permission", "holdout_pending", "split_proposed", "budget-family",
}

// The summary repeats the narration's prose (it may not import the factory): a reason added to
// one must reach the other.
func TestSummaryReasonsTrackNarrate(t *testing.T) {
	have := map[string]bool{}
	for _, r := range summ.Reasons() {
		have[r] = true
		if _, ok := reasons()[r]; !ok {
			t.Errorf("summary has prose for %q, which narrate does not know: renamed or removed?", r)
		}
	}
	for _, r := range waitReasons {
		if _, ok := reasons()[r]; !ok {
			t.Errorf("narrate does not know the wait reason %q", r)
		}
		// waiting_* holds are summary.holds(), not Reasons().
		if !have[r] && !strings.HasPrefix(r, "waiting_") {
			t.Errorf("the summary has no prose for the wait reason %q", r)
		}
	}
}

// #51: the comment that hands a PR to the maintainer says how to merge under the ruleset.
func TestWaitingForHumanSaysHowToMerge(t *testing.T) {
	tk := task()
	tk.Spec.Repository = "Smana/cloud-native-ref"
	tk.Status.PullRequest = &v1alpha1.PullRequestRef{Number: 2267, URL: "https://github.com/Smana/cloud-native-ref/pull/2267"}
	body := WaitingForHuman(tk, "policy_pending").Body
	if !strings.Contains(body, "gh pr merge 2267 --squash --admin --repo Smana/cloud-native-ref") || !strings.Contains(body, "bypass") {
		t.Fatalf("no merge instruction: %s", body)
	}
	tk.Status.PullRequest = nil
	if body := WaitingForHuman(tk, "policy_pending").Body; strings.Contains(body, "gh pr merge") {
		t.Fatalf("no PR, no merge instruction: %s", body)
	}
}

// WouldArmAdopted names a fence, sizes and a release age only when the class declares them.
func TestWouldArmAdoptedNamesWhatIsDeclared(t *testing.T) {
	tk := &v1alpha1.Task{}
	bare := WouldArmAdopted(tk, config.Class{}, "deps-patch", "h").Body
	full := WouldArmAdopted(tk, config.Class{AllowedFiles: []string{"x"}, ReleaseAgeCheck: "age"}, "deps-patch", "h").Body
	if strings.Contains(bare, "files and sizes") || strings.Contains(bare, "release-age") ||
		!strings.Contains(full, "files and sizes") || !strings.Contains(full, "release-age check") {
		t.Fatalf("bare %q full %q", bare, full)
	}
}
