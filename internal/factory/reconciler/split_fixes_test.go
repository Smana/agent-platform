// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"time"

	"context"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/forge"
	"github.com/Smana/agent-platform/internal/factory/narrate"
)

// A part carries its parent's search, so its implementer claim does too.
func TestAPartInheritsSearch(t *testing.T) {
	g := splitRigWith(t, "frontier", false, nil)
	g.r.Cfg.Loops = map[string]config.Loop{"review": {Scope: config.ScopeScoper, Search: true, Split: &config.SplitPolicy{MaxChildren: 3}}}
	g.reconcile(t, "3buqdlot", 3)
	splitHandoffOf(g, 0, readyScope(), parts(2))
	endScoper(t, g, "Succeeded", "agent_finished")
	g.r.Now = func() time.Time { return now.Add(time.Hour) }
	g.f.SetComments(7, splitBy(81, "Smana"))
	g.reconcile(t, "3buqdlot", 2)
	g.reconcile(t, child(1), 3) // Received → Triaged → Queued → its implementer claim
	c := childTask(t, g, 1)
	if !c.Spec.Search {
		t.Fatal("the part lost spec.search")
	}
	found := false
	for _, s := range g.runs.specs {
		found = found || s.Search
	}
	if !found {
		t.Fatalf("no claim carries search: %+v", g.runs.specs)
	}
}

// conflictingParent makes the parent's status write fail with a conflict n times (-1: always).
func conflictingParent(g *rig, n int) {
	g.r.Client = interceptor.NewClient(g.c.(client.WithWatch), interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, o client.Object, opts ...client.SubResourceUpdateOption) error {
		if o.GetName() == "3buqdlot" && n != 0 {
			n--
			return apierrors.NewConflict(schema.GroupResource{Group: "agents.ogenki.io", Resource: "tasks"}, o.GetName(), nil)
		}
		return c.SubResource(sub).Update(ctx, o, opts...)
	}})
}

// A conflicting write on the parent never fails revert(): GitHub's revert is not idempotent, so the
// revert PR number must reach the status, and the next pass must not open a second one.
func TestATellParentConflictNeverOpensASecondRevert(t *testing.T) {
	for name, n := range map[string]int{"once": 1, "always": -1} {
		t.Run(name, func(t *testing.T) {
			g, c := openRevertRig(t)
			var p v1alpha1.Task
			_ = g.c.Get(t.Context(), taskKey("3buqdlot"), &p)
			p.Status.Phase, p.Status.Reason = v1alpha1.PhaseEscalated, "budget-family"
			if err := g.c.Status().Update(t.Context(), &p); err != nil {
				t.Fatal(err)
			}
			conflictingParent(g, n)
			if err := g.r.revert(t.Context(), c, "main_red"); err != nil {
				t.Fatalf("revert failed on the parent's conflict: %v", err)
			}
			if c.Status.PullRequest.RevertNumber == 0 || c.Status.Phase != v1alpha1.PhaseReverted || len(g.f.Reverts()) != 1 {
				t.Fatalf("%+v %s %v", c.Status.PullRequest, c.Status.Phase, g.f.Reverts())
			}
			if err := g.r.revert(t.Context(), c, "main_red"); err != nil || len(g.f.Reverts()) != 1 {
				t.Fatalf("a replay opened another: %v %v", err, g.f.Reverts())
			}
		})
	}
}

// The wording follows the parent: "stays closed" only when it is closed.
func TestPartRevertedWordsTheParentsState(t *testing.T) {
	tk := &v1alpha1.Task{}
	tk.Name, tk.Spec.Issue = "chldpart", 501
	tk.Spec.Parent = &v1alpha1.Parent{Task: "3buqdlot", Issue: 7, Index: 1, Of: 2}
	if b := narrate.PartReverted(tk, 901, true).Body; !strings.Contains(b, "stays closed") {
		t.Fatal(b)
	}
	if b := narrate.PartReverted(tk, 901, false).Body; strings.Contains(b, "stays closed") || !strings.Contains(b, "waiting") {
		t.Fatal(b)
	}
}

// A part whose PR already merged finishes its watch when the family ends.
func TestAStoppedFamilyLetsAMergedPartFinish(t *testing.T) {
	g := approved(t)
	for _, ph := range []string{v1alpha1.PhaseMerged, v1alpha1.PhaseVerifying} {
		setPhase(t, g, child(1), ph, "")
		var c v1alpha1.Task
		_ = g.c.Get(t.Context(), taskKey(child(1)), &c)
		merged := metav1.NewTime(now)
		c.Status.PullRequest.MergedAt = &merged
		_ = g.c.Status().Update(t.Context(), &c)
		var p v1alpha1.Task
		_ = g.c.Get(t.Context(), taskKey("3buqdlot"), &p)
		newest, _, _ := g.r.family(t.Context(), &p)
		if err := g.r.stopOpenParts(t.Context(), p.Status.Split, newest); err != nil {
			t.Fatal(err)
		}
		if c := childTask(t, g, 1); c.Annotations[v1alpha1.AnnotationStop] != "" || p.Status.Split.Children[0].Outcome != "" {
			t.Fatalf("%s: stopped a merged part: %v %q", ph, c.Annotations, p.Status.Split.Children[0].Outcome)
		}
	}
}

// A stop annotation on a parent waiting in Split stops its open parts too.
func TestAStopOnTheParentStopsItsParts(t *testing.T) {
	g := approved(t)
	var p v1alpha1.Task
	_ = g.c.Get(t.Context(), taskKey("3buqdlot"), &p)
	if p.Status.Phase != v1alpha1.PhaseSplit {
		t.Fatal(p.Status.Phase)
	}
	if p.Annotations == nil {
		p.Annotations = map[string]string{}
	}
	p.Annotations[v1alpha1.AnnotationStop] = "true"
	if err := g.c.Update(t.Context(), &p); err != nil {
		t.Fatal(err)
	}
	got := g.reconcile(t, "3buqdlot", 1)
	if got.Status.Phase != v1alpha1.PhaseStopped {
		t.Fatal(got.Status.Phase)
	}
	if c := childTask(t, g, 1); c.Annotations[v1alpha1.AnnotationStop] != StopFamilyDropped {
		t.Fatalf("the open part kept running: %v", c.Annotations)
	}
	if got.Status.Split.Children[0].Outcome != ChildDropped {
		t.Fatalf("%+v", got.Status.Split.Children)
	}
}

// The judge reads the part's checks first: in a part's brief the scope comes before the steering.
func TestAPartsBriefPutsItsScopeBeforeTheSteering(t *testing.T) {
	tk := partTask(parentText)
	b := firstBrief(tk, "n0nce234", partScoped(tk))
	if sp, st := strings.Index(b, "SCOPE-DATA-"), strings.Index(b, "STEERING-"); sp < 0 || st < 0 || sp > st {
		t.Fatalf("scope %d steering %d", sp, st)
	}
}

// A part's pull request that closes its parent's issue is never armed: the parent closes with its last part.
func TestAPartClosingItsParentIsNotArmed(t *testing.T) {
	for _, kw := range []string{"Fixes", "closes", "Resolved"} {
		g := mergeRig(t, v1alpha1.PhaseAwaitingCI)
		var tk v1alpha1.Task
		_ = g.c.Get(t.Context(), taskKey("3buqdlot"), &tk)
		tk.Spec.Parent = &v1alpha1.Parent{Task: "paarent2", Issue: 5, Index: 1, Of: 2}
		_ = g.c.Update(t.Context(), &tk)
		g.f.SetPR(forge.PR{Number: 12, NodeID: "PR_12", State: "OPEN", Title: "docs: fix a link", Author: "ogenki-agents[bot]",
			HeadRef: "agent/3buqdlot", HeadSHA: "abc", HeadMessage: "docs: fix\n\nAgent-Run: " + rid(0), Body: "Does a part.\n\n" + kw + " #5\nFixes #7"})
		g.f.SetChecks(12, green("SUCCESS"))
		got := g.reconcile(t, "3buqdlot", 1)
		if got.Status.Phase != v1alpha1.PhaseAwaitingHuman || got.Status.Reason != "closes_parent" || len(g.f.Armed()) != 0 {
			t.Fatalf("%s: %s %s %v", kw, got.Status.Phase, got.Status.Reason, g.f.Armed())
		}
	}
	g := mergeRig(t, v1alpha1.PhaseAwaitingCI) // a body naming another issue arms as before
	var tk v1alpha1.Task
	_ = g.c.Get(t.Context(), taskKey("3buqdlot"), &tk)
	tk.Spec.Parent = &v1alpha1.Parent{Task: "paarent2", Issue: 5, Index: 1, Of: 2}
	_ = g.c.Update(t.Context(), &tk)
	g.f.SetPR(forge.PR{Number: 12, NodeID: "PR_12", State: "OPEN", Title: "docs: fix a link", Author: "ogenki-agents[bot]",
		HeadRef: "agent/3buqdlot", HeadSHA: "abc", HeadMessage: "docs: fix\n\nAgent-Run: " + rid(0), Body: "Fixes #7, see #5 and #50"})
	g.f.SetChecks(12, green("SUCCESS"))
	if got := g.reconcile(t, "3buqdlot", 1); got.Status.Phase != v1alpha1.PhaseAutoMerging {
		t.Fatalf("%s %s", got.Status.Phase, got.Status.Reason)
	}
}
