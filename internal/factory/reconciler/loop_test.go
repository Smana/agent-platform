// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"slices"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"

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

// The loop row reaches implementer claims only, on every run of the task, as triage recorded it.
func TestALoopRowReachesImplementerClaimsOnly(t *testing.T) {
	steps := int32(5)
	triaged := runs.Loop{StallSteps: 40, Continuations: 2, JudgeModel: "tier-light"}
	for _, tc := range []struct {
		name, role, trigger string
		changeConfig        bool
		want                *runs.Loop
	}{
		{"reviewer", "reviewer", "verifier", false, nil},
		{"tester", "tester", "verifier", false, nil},
		{"triager", "triager", "triage", false, nil},
		{"implementer revision", "implementer", "revision", false, &triaged},
		{"implementer CI fix", "implementer", "ci", false, &triaged},
		{"implementer retry", "implementer", "retry", false, &triaged},
		{"config row changed after triage", "implementer", "revision", true, &triaged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, tk := loopRig(t)
			if tc.changeConfig {
				g.r.Cfg.Loops = map[string]config.Loop{"review": {StallSteps: &steps}}
			}
			s := g.r.implementerSpec(tk, "text")
			s.Role = tc.role
			before := *tk.Spec.Loop
			if err := g.r.startRun(t.Context(), tk, s, tc.trigger); err != nil {
				t.Fatal(err)
			}
			got := g.runs.specs[runID(tk.Name, len(tk.Status.Runs)-1)].Loop
			if (got == nil) != (tc.want == nil) || got != nil && *got != *tc.want {
				t.Fatalf("claim loop %+v, want %+v", got, tc.want)
			}
			if *tk.Spec.Loop != before {
				t.Fatalf("task loop moved: %+v", tk.Spec.Loop)
			}
		})
	}
}

// Final review M1: a run lost to its sandbox and resumed is not a loop run yet; only the run that ends counts.
func TestALostLoopRunIsNotCountedBeforeItsResume(t *testing.T) {
	g, _ := loopRig(t)
	g.lose(rid(0), runs.ReasonDisrupted)
	tk := g.reconcile(t, "3buqdlot", 2)
	if tk.Status.Resumes != 1 {
		t.Fatalf("resumes %d, want 1", tk.Status.Resumes)
	}
	for _, m := range g.metrics.recorded {
		if strings.HasPrefix(m, "loop_run") {
			t.Fatalf("counted %v before the run that ended", g.metrics.recorded)
		}
	}
}

// A search-only claim (no loop row) the cluster refuses escalates the same way.
func TestARefusedSearchClaimEscalates(t *testing.T) {
	g := newRig(t, issueTask("3buqdlot", 7, "# Fix the link"))
	g.r.Cfg.Loops = map[string]config.Loop{"review": {Search: true}}
	creates := 0
	g.runs.refuse = func(s runs.Spec) error {
		if s.Role != "implementer" {
			return nil
		}
		creates++
		return apierrors.NewInvalid(schema.GroupKind{Group: "agents.ogenki.io", Kind: "AgentRun"}, "x", field.ErrorList{field.Invalid(field.NewPath("spec", "loop", "search"), "x", "unknown field")})
	}
	tk := g.reconcile(t, "3buqdlot", 4)
	if tk.Status.Phase != v1alpha1.PhaseEscalated || tk.Status.Reason != "loop_unsupported" || creates != 1 {
		t.Fatalf("%s %s after %d creates, want Escalated loop_unsupported after 1", tk.Status.Phase, tk.Status.Reason, creates)
	}
}

// A loop claim the cluster refuses (the host XRD lacks spec.loop, Strict validation) escalates the task
// as loop_unsupported, with no run and no retry; a refused claim without a loop keeps the error path.
func TestARefusedLoopClaimEscalates(t *testing.T) {
	g := newRig(t, issueTask("3buqdlot", 7, "# Fix the link"))
	steps := int32(40)
	g.r.Cfg.Loops = map[string]config.Loop{"review": {StallSteps: &steps}}
	creates := 0
	g.runs.refuse = func(s runs.Spec) error {
		if s.Role != "implementer" {
			return nil
		}
		creates++
		return apierrors.NewInvalid(schema.GroupKind{Group: "agents.ogenki.io", Kind: "AgentRun"}, "x", field.ErrorList{field.Invalid(field.NewPath("spec", "loop"), "x", "unknown field")})
	}
	tk := g.reconcile(t, "3buqdlot", 4)
	if tk.Status.Phase != v1alpha1.PhaseEscalated || tk.Status.Reason != "loop_unsupported" {
		t.Fatalf("%s %s, want Escalated loop_unsupported", tk.Status.Phase, tk.Status.Reason)
	}
	if creates != 1 {
		t.Fatalf("%d creates, want 1: no retry", creates)
	}
	if fit := Fit(tk); fit != "" {
		t.Fatalf("tier scored %q", fit)
	}
}
