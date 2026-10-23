// SPDX-License-Identifier: Apache-2.0

package runctl

import (
	"testing"

	v1alpha1 "github.com/Smana/agent-platform/api/v1alpha1"
)

func bridgeEnv(t *testing.T, r *v1alpha1.AgentRun) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, c := range Pod(r, testConfig(t), testModel, "abcdefgh").Spec.InitContainers {
		if c.Name == "room-bridge" {
			for _, e := range c.Env {
				out[e.Name] = e.Value
			}
			return out
		}
	}
	t.Fatal("no room-bridge container")
	return nil
}

// v3 feature 5: spec.loop.search hands an implementer's harness and bridge AGENT_SEARCH=1; without it,
// or for another role, the pod is today's (the harness offers the tool to an implementer only).
func TestLoopSearchReachesTheHarnessAndTheBridge(t *testing.T) {
	on := run("run-abcdefgh", func(r *v1alpha1.AgentRun) { r.Spec.Loop.Search = true })
	if env := harnessEnv(Pod(on, testConfig(t), testModel, "abcdefgh")); env["AGENT_SEARCH"] != "1" {
		t.Fatalf("harness AGENT_SEARCH %q, want 1", env["AGENT_SEARCH"])
	}
	if env := bridgeEnv(t, on); env["AGENT_SEARCH"] != "1" {
		t.Fatalf("bridge AGENT_SEARCH %q, want 1", env["AGENT_SEARCH"])
	}
	off := run("run-abcdefgh")
	tester := run("run-abcdefgh", func(r *v1alpha1.AgentRun) {
		r.Spec.Loop.Search = true
		r.Spec.Role = "tester"
	})
	for name, r := range map[string]*v1alpha1.AgentRun{"no search": off, "tester": tester} {
		if _, ok := harnessEnv(Pod(r, testConfig(t), testModel, "abcdefgh"))["AGENT_SEARCH"]; ok {
			t.Fatalf("%s: AGENT_SEARCH on the harness", name)
		}
		if _, ok := bridgeEnv(t, r)["AGENT_SEARCH"]; ok {
			t.Fatalf("%s: AGENT_SEARCH on the bridge", name)
		}
	}
}
