// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"slices"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/runs"
)

// testerDue (F3-R1): the class writes its tests first, the task is public and holds no contract, and no
// tester ran yet, or the last one was lost and resumes. Records, not a status field, say the stage ran:
// a /factory retry after the tester's end does not run it twice.
func (r *Reconciler) testerDue(t *v1alpha1.Task) bool {
	if !r.repo(t).TestFirstFor(t.Spec.PredictedClass) || t.Spec.DataClass != "public" || t.Status.Contract != nil ||
		!slices.Contains(r.Cfg.Templates[t.Spec.Template].Roles, "implementer") {
		return false
	}
	if len(t.Status.Runs) > 0 && current(t).Role == "tester" {
		return nextTrigger(t) == "resume"
	}
	return !slices.ContainsFunc(t.Status.Runs, func(x v1alpha1.RunRecord) bool { return x.Role == "tester" })
}

// testerRun: the task's current run is its tester's.
func testerRun(t *v1alpha1.Task) bool { return len(t.Status.Runs) > 0 && current(t).Role == "tester" }

// testerSpec (F3-R2): the implementer's claim, on the task's own model and caps (R19), as a tester.
func (r *Reconciler) testerSpec(t *v1alpha1.Task, text string) runs.Spec {
	s := r.implementerSpec(t, text)
	s.Role = "tester"
	r.withChecks(&s, t)
	return s
}

// startTester starts the tester run, or resumes a lost one with its own brief.
func (r *Reconciler) startTester(ctx context.Context, t *v1alpha1.Task) error {
	text, ok, err := r.resumeBrief(ctx, t)
	if err != nil {
		return err
	}
	if !ok {
		sc, err := r.taskScope(ctx, t)
		if err != nil {
			return err
		}
		text = TesterBrief(t, r.Nonce(), sc)
	}
	return r.startRun(ctx, t, r.testerSpec(t, text), nextTrigger(t))
}
