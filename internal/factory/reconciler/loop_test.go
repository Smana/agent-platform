// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"testing"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/runs"
)

// Loop policy part 2, D4: a class with no row gets today's claim, with no spec.loop.
func TestNoLoopRowIsTodaysRun(t *testing.T) {
	g := newRig(t, issueTask("3buqdlot", 7, "# Fix the link"))
	tk := g.reconcile(t, "3buqdlot", 3)
	if tk.Spec.Loop != nil || g.runs.specs[rid(0)].Loop != nil {
		t.Fatalf("task %+v, claim %+v: want no loop", tk.Spec.Loop, g.runs.specs[rid(0)].Loop)
	}
}

// loopRig is a rig whose review class (the rig's default) has a loop row read at triage.
func loopRig(t *testing.T) (*rig, *v1alpha1.Task) {
	t.Helper()
	g := newRig(t, issueTask("3buqdlot", 7, "# Fix the link"))
	steps := int32(40)
	g.r.Cfg.Loops = map[string]config.Loop{"review": {StallSteps: &steps}}
	return g, g.reconcile(t, "3buqdlot", 3)
}

// The row read at triage reaches the task and the implementer's claim, every value resolved.
func TestALoopRowReachesTheImplementersClaim(t *testing.T) {
	g, tk := loopRig(t)
	if tk.Spec.Loop == nil || *tk.Spec.Loop != (v1alpha1.Loop{StallSteps: 40, Continuations: 2, JudgeModel: "tier-light"}) {
		t.Fatalf("task loop %+v", tk.Spec.Loop)
	}
	if l := g.runs.specs[rid(0)].Loop; l == nil || *l != (runs.Loop{StallSteps: 40, Continuations: 2, JudgeModel: "tier-light"}) {
		t.Fatalf("claim loop %+v", l)
	}
}
