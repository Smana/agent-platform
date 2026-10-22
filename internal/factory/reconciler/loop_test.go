// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"slices"
	"strings"
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

// A loop end becomes the task's reason, its narration (the category, no judge prose) and its
// counter, whatever the config says after triage.
func TestALoopEndEscalatesWithItsReason(t *testing.T) {
	prose := map[string]string{"judge_impossible": "cannot be done as asked", "stalled": "made no commit",
		"not_met": "after the last continuation"}
	for end, words := range prose {
		t.Run(end, func(t *testing.T) {
			g, _ := loopRig(t)
			g.r.Cfg.Loops = nil // a config change never moves a triaged task
			g.runs.set(rid(0), "Failed")
			g.log.end(rid(0), "Failed", end)
			tk := g.reconcile(t, "3buqdlot", 1)
			if tk.Status.Phase != v1alpha1.PhaseEscalated || tk.Status.Reason != end {
				t.Fatalf("%s %s, want Escalated %s", tk.Status.Phase, tk.Status.Reason, end)
			}
			if c := strings.Join(g.f.Comments(7), "\n"); !strings.Contains(c, words) {
				t.Fatalf("the narration names the category: %q", c)
			}
			if !slices.Contains(g.metrics.recorded, "loop_run review "+end) {
				t.Fatalf("metrics %v", g.metrics.recorded)
			}
		})
	}
}
