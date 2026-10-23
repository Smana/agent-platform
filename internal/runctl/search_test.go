// SPDX-License-Identifier: Apache-2.0

package runctl

import (
	"testing"

	v1alpha1 "github.com/Smana/agent-platform/api/v1alpha1"
)

// v3 feature 5: spec.loop.search hands the harness AGENT_SEARCH=1; without it the pod is today's.
func TestLoopSearchReachesTheHarness(t *testing.T) {
	on := run("run-abcdefgh", func(r *v1alpha1.AgentRun) { r.Spec.Loop.Search = true })
	if env := harnessEnv(Pod(on, testConfig(t), testModel, "abcdefgh")); env["AGENT_SEARCH"] != "1" {
		t.Fatalf("AGENT_SEARCH %q, want 1", env["AGENT_SEARCH"])
	}
	if _, ok := harnessEnv(Pod(run("run-abcdefgh"), testConfig(t), testModel, "abcdefgh"))["AGENT_SEARCH"]; ok {
		t.Fatal("AGENT_SEARCH set on a run without search")
	}
}
