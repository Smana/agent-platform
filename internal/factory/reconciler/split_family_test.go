// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/runs"
	"github.com/Smana/agent-platform/internal/factory/taskid"
)

// child is part k's first Task name.
func child(k int) string { return taskid.Name(taskid.SplitKey("3buqdlot", k)) }

// childTask reads part k's first Task.
func childTask(t *testing.T, g *rig, k int) *v1alpha1.Task {
	t.Helper()
	var tk v1alpha1.Task
	if err := g.c.Get(t.Context(), taskKey(child(k)), &tk); err != nil {
		t.Fatalf("part %d: %v", k, err)
	}
	return &tk
}

// approved is a proposed split a maintainer approved, after the parent's pass that opens part 1.
func approved(t *testing.T) *rig {
	t.Helper()
	g := proposed(t)
	g.r.Now = func() time.Time { return now.Add(time.Hour) }
	g.f.SetComments(7, splitBy(81, "Smana"))
	g.reconcile(t, "3buqdlot", 2) // Escalated → Split, then part 1
	return g
}

// SL7, SL8, offline proof 4: approval opens part 1 only: its issue with the title, the marker and
// both labels, its sub-issue link, its Task with the parent, the scope, and the approver as payer.
func TestApprovalOpensPartOneOnly(t *testing.T) {
	g := approved(t)
	created := g.f.Created()
	if len(created) != 1 || !strings.HasPrefix(created[0], "Part 1 of 2 of #7\n") || !strings.Contains(created[0], "split=3buqdlot/1") ||
		!strings.HasSuffix(created[0], "labels: factory/child-of:#7") || !strings.Contains(created[0], "on page 1") {
		t.Fatalf("issues %q", created)
	}
	if !slices.Contains(g.f.Calls(), "sub-issue 7 9501") {
		t.Fatalf("calls %v", g.f.Calls())
	}
	p := g.reconcile(t, "3buqdlot", 0)
	if p.Status.Phase != v1alpha1.PhaseSplit || len(p.Status.Split.Children) != 1 ||
		p.Status.Split.Children[0] != (v1alpha1.SplitChild{Index: 1, Issue: 501, Task: child(1)}) {
		t.Fatalf("%s %+v", p.Status.Phase, p.Status.Split)
	}
	c := childTask(t, g, 1)
	if *c.Spec.Parent != (v1alpha1.Parent{Task: "3buqdlot", Issue: 7, Index: 1, Of: 2}) || c.Spec.Issue != 501 ||
		c.Spec.Source.RequestedBy != "github:smana" || c.Labels[v1alpha1.LabelParent] != "3buqdlot" || c.Labels[v1alpha1.LabelIssue] != "501" ||
		c.Spec.Scope == nil || !strings.Contains(c.Spec.Scope.Task, "on page 1") {
		t.Fatalf("part 1 %+v %v", c.Spec, c.Labels)
	}
	var l v1alpha1.TaskList
	_ = g.c.List(t.Context(), &l)
	if len(l.Items) != 2 {
		t.Fatalf("one part at a time (SL7): %d tasks", len(l.Items))
	}
}

// SL5, SL9, SL10, SL11: a child's triage gives it its parent's text, class and steering, its scope,
// pair at standard, the family's cap, and no classifier call; its runs are the approver's.
func TestAChildIsTriagedFromItsParent(t *testing.T) {
	g := approved(t)
	g.reconcile(t, child(1), 3) // Received → Triaged → Queued → the implementer
	c := childTask(t, g, 1)
	p := g.reconcile(t, "3buqdlot", 0)
	if c.Spec.Text != parentText || c.Spec.Source.ContentSHA256 != p.Spec.Source.ContentSHA256 || c.Spec.PredictedClass != "review" ||
		c.Spec.ScopeMode != "scoper" || c.Spec.Template != "pair" || c.Spec.Budget.Tier != "standard" ||
		c.Spec.Budget.TaskMicroUSD != 5_100_000 || c.Status.Classification.Classifier != "split" || c.Status.Classification.Fit != "" {
		t.Fatalf("part 1 %+v %+v", c.Spec, c.Status.Classification)
	}
	s := g.runs.specs[runID(child(1), 0)]
	if s.Role != "implementer" || s.Branch != "agent/"+child(1) {
		t.Fatalf("no scoper for a part (SL5), its own branch: %+v", s)
	}
}

// SR10, offline proof 4: a replay after a lost write following CreateIssue adopts the issue by its
// marker and opens none; the Task's AlreadyExists is the dedup.
func TestAReplayOpensNoSecondIssue(t *testing.T) {
	g := approved(t)
	p := g.reconcile(t, "3buqdlot", 0)
	p.Status.Split.Children = nil // the write that recorded part 1 was lost
	if err := g.c.Status().Update(t.Context(), p); err != nil {
		t.Fatal(err)
	}
	p = g.reconcile(t, "3buqdlot", 1)
	if len(g.f.Created()) != 1 || len(p.Status.Split.Children) != 1 || p.Status.Split.Children[0].Issue != 501 {
		t.Fatalf("issues %q children %+v", g.f.Created(), p.Status.Split.Children)
	}
}

// SL8: a failed sub-issue link blocks nothing.
func TestAFailedSubIssueLinkBlocksNothing(t *testing.T) {
	g := proposed(t)
	g.f.FailSubIssues(errors.New("422 sub-issues disabled"))
	g.r.Now = func() time.Time { return now.Add(time.Hour) }
	g.f.SetComments(7, splitBy(81, "Smana"))
	if p := g.reconcile(t, "3buqdlot", 2); len(p.Status.Split.Children) != 1 {
		t.Fatalf("children %+v", p.Status.Split.Children)
	}
}

// SL11: the approver pays, not the parent's labeller: a part's requestedBy is the approver's, and
// with per-person caps on its runs are charged to them.
func TestTheApproverPaysForTheParts(t *testing.T) {
	g := splitRigWith(t, "frontier", false, func(tk *v1alpha1.Task) { tk.Spec.Source.RequestedBy = "github:alice" })
	g.r.Cfg.Budgets = personBudgets(true, false)
	g.reconcile(t, "3buqdlot", 3)
	splitHandoffOf(g, 0, readyScope(), parts(2))
	endScoper(t, g, "Succeeded", "agent_finished")
	g.r.Now = func() time.Time { return now.Add(time.Hour) }
	g.f.SetComments(7, splitBy(81, "Smana"))
	g.reconcile(t, "3buqdlot", 2)
	c := childTask(t, g, 1)
	if c.Spec.Source.RequestedBy != "github:smana" || c.Payer() != "github:smana" || runs.TaskPayer(c, true) != "github:smana" {
		t.Fatalf("requestedBy %q payer %q", c.Spec.Source.RequestedBy, c.Payer())
	}
}
