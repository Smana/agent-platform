// SPDX-License-Identifier: Apache-2.0

package narrate

import (
	"strings"
	"testing"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
)

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
