// SPDX-License-Identifier: Apache-2.0

package runctl

import (
	"fmt"

	v1alpha1 "github.com/Smana/agent-platform/api/v1alpha1"
)

// Rules is the run's rules.md: the composition's _rulesFor without the lines that name
// room_* tools or the model gateway, neither of which this runtime has (3a R8).
func Rules(r *v1alpha1.AgentRun, runID string) string {
	s := r.Spec
	branch := Branch(r, runID)
	out := fmt.Sprintf("Platform rules for run %s (%s) on %s. The platform enforces each of them independently of you.\n"+
		"1. Work in /workspace/repo, cloned from %s at %s.\n", runID, s.Role, s.Repository, s.Repository, s.BaseRef)
	if s.Role == "implementer" {
		out += fmt.Sprintf("2. Push only to %s. Never push to any other branch, and never merge.\n"+
			"3. Open at most one pull request, from %s into %s. If it already exists, update it.\n"+
			"4. Every commit carries the trailer \"Agent-Run: %s\". The commit hook adds it.\n"+
			"5. Never modify .github/workflows/, .github/chainguard/ or .github/agent-factory.yaml.\n",
			branch, branch, s.BaseRef, runID)
	} else {
		out += "2. Your repository access is read-only. Do not push. Your final message is your report.\n" +
			"3. Never modify .github/workflows/, .github/chainguard/ or .github/agent-factory.yaml.\n"
	}
	if s.RoomRef == "" {
		return out
	}
	out += fmt.Sprintf("\nRoom %s:\n- Everything you do is recorded in the room's log, and humans may be watching.\n", s.RoomRef)
	if s.Role == "implementer" {
		out += "- Open the pull request with gh pr create: it appends this run's provenance footer (Agent-Room, Agent-Run). Keep the footer.\n"
	}
	return out
}
