// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/forge"
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

// setPhase writes a task's phase and reason as its own reconcile would have.
func setPhase(t *testing.T, g *rig, name, phase, reason string) {
	t.Helper()
	var tk v1alpha1.Task
	if err := g.c.Get(t.Context(), taskKey(name), &tk); err != nil {
		t.Fatal(err)
	}
	tk.Status.Phase, tk.Status.Reason = phase, reason
	tk.Status.PullRequest = &v1alpha1.PullRequestRef{Number: 600 + len(name), URL: "https://github.com/Smana/cloud-native-ref/pull/" + strconv.Itoa(600+len(name))}
	if err := g.c.Status().Update(t.Context(), &tk); err != nil {
		t.Fatal(err)
	}
}

// SL7, SL12, offline proof 5: part 1 Done opens part 2 with min(5.10, left); part 2 Done closes the
// parent issue and ends the parent Done, children_merged, counting each part once.
func TestThePartsLandOneByOneAndCloseTheParent(t *testing.T) {
	g := approved(t)
	setPhase(t, g, child(1), v1alpha1.PhaseDone, "merged")
	p := g.reconcile(t, "3buqdlot", 1)
	if len(p.Status.Split.Children) != 2 || p.Status.Split.Children[0].Outcome != "landed" || len(g.f.Created()) != 2 ||
		!strings.HasPrefix(g.f.Created()[1], "Part 2 of 2 of #7\n") {
		t.Fatalf("%+v %q", p.Status.Split, g.f.Created())
	}
	g.reconcile(t, child(2), 1)
	if c := childTask(t, g, 2); c.Spec.Budget.TaskMicroUSD != 5_100_000 {
		t.Fatalf("part 2's cap %d", c.Spec.Budget.TaskMicroUSD)
	}
	setPhase(t, g, child(2), v1alpha1.PhaseDone, "merged")
	p = g.reconcile(t, "3buqdlot", 1)
	if p.Status.Phase != v1alpha1.PhaseDone || p.Status.Reason != "children_merged" || !slices.Contains(g.f.Calls(), "close-issue 7") {
		t.Fatalf("%s %s %v", p.Status.Phase, p.Status.Reason, g.f.Calls())
	}
	if n := strings.Count(strings.Join(g.metrics.recorded, "|"), "split_child review landed"); n != 2 || p.Status.Classification.Fit != "" {
		t.Fatalf("counted %d, fit %q: %v", n, p.Status.Classification.Fit, g.metrics.recorded)
	}
	g.r.Now = func() time.Time { return now.Add(time.Hour + g.r.settleWindow()) }
	g.reconcile(t, "3buqdlot", 1)
	if last := g.f.Comments(7)[len(g.f.Comments(7))-1]; !strings.Contains(last, "every part of the split merged") || !strings.Contains(last, "#501") {
		t.Fatalf("the parent's end lists its parts: %q", last)
	}
}

// SL12: a part's issue closed with no landed task, or a part reverted, ends the parent Closed,
// child_dropped, and opens no further part.
func TestADroppedOrRevertedPartEndsTheFamily(t *testing.T) {
	g := approved(t)
	setPhase(t, g, child(1), v1alpha1.PhaseStopped, "stopped_by_label")
	if p := g.reconcile(t, "3buqdlot", 1); p.Status.Phase != v1alpha1.PhaseSplit {
		t.Fatalf("a stopped part with its issue open keeps the parent waiting (SL12): %s", p.Status.Phase)
	}
	_ = g.f.CloseIssue(t.Context(), 501)
	p := g.reconcile(t, "3buqdlot", 1)
	if p.Status.Phase != v1alpha1.PhaseClosed || p.Status.Reason != "child_dropped" || p.Status.Split.Children[0].Outcome != "dropped" ||
		len(g.f.Created()) != 1 || slices.Contains(g.f.Calls(), "close-issue 7") || !slices.Contains(g.metrics.recorded, "split_child review dropped") {
		t.Fatalf("%s %s %+v %v", p.Status.Phase, p.Status.Reason, p.Status.Split, g.metrics.recorded)
	}
	g = approved(t)
	setPhase(t, g, child(1), v1alpha1.PhaseDone, "merged")
	g.reconcile(t, "3buqdlot", 1) // part 2 opens
	setPhase(t, g, child(1), v1alpha1.PhaseReverted, "main_red")
	p = g.reconcile(t, "3buqdlot", 1)
	if p.Status.Phase != v1alpha1.PhaseClosed || p.Status.Split.Children[0].Outcome != "reverted" {
		t.Fatalf("%s %+v", p.Status.Phase, p.Status.Split)
	}
	// The running part 2 must not outlive the family: stopped, its outcome recorded and counted.
	if len(p.Status.Split.Children) != 2 || p.Status.Split.Children[1].Outcome != "dropped" ||
		childTask(t, g, 2).Annotations[v1alpha1.AnnotationStop] == "" || !slices.Contains(g.metrics.recorded, "split_child review dropped") {
		t.Fatalf("%+v %v", p.Status.Split, childTask(t, g, 2).Annotations)
	}
	if c := g.reconcile(t, child(2), 1); c.Status.Phase != v1alpha1.PhaseStopped {
		t.Fatalf("part 2: %s", c.Status.Phase)
	}
}

// A task labelled as a child that names another parent is no part of this family: its spend is not
// charged to the family's cap (the label is anyone's, the spec is CEL's).
func TestAStrayLabelledTaskIsNotInTheFamily(t *testing.T) {
	g := approved(t)
	before, err := g.r.familyLeft(t.Context(), g.reconcile(t, "3buqdlot", 0))
	if err != nil {
		t.Fatal(err)
	}
	stray := &v1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "zzstray", Namespace: "agent-system", Labels: map[string]string{v1alpha1.LabelParent: "3buqdlot"}},
		Spec: v1alpha1.TaskSpec{Parent: &v1alpha1.Parent{Task: "other", Issue: 9, Index: 1, Of: 2}}}
	if err := g.c.Create(t.Context(), stray); err != nil {
		t.Fatal(err)
	}
	stray.Status.Runs = []v1alpha1.RunRecord{{ID: "x", Role: "implementer", CostMicroUSD: 5_000_000, Tokens: 1}}
	if err := g.c.Status().Update(t.Context(), stray); err != nil {
		t.Fatal(err)
	}
	if after, err := g.r.familyLeft(t.Context(), g.reconcile(t, "3buqdlot", 0)); err != nil || after != before {
		t.Fatalf("left %d then %d (%v)", before, after, err)
	}
}

// A part whose run falls back to token caps (an unpriced run) is bounded in tokens by what the
// family has left at the reference rate, the rate spend() charges such a run: the USD cap alone
// would let it spend past the family's.
func TestAPartsTokenCapFollowsWhatTheFamilyHasLeft(t *testing.T) {
	g := approved(t)
	var c v1alpha1.Task
	_ = g.c.Get(t.Context(), taskKey(child(1)), &c)
	c.Status.Phase, c.Status.Reason = v1alpha1.PhaseDone, "merged"
	c.Status.Runs = []v1alpha1.RunRecord{{ID: runID(child(1), 0), Role: "implementer", Trigger: "initial", CostMicroUSD: 17_000_000, Tokens: 1}}
	_ = g.c.Status().Update(t.Context(), &c)
	p := g.reconcile(t, "3buqdlot", 1)
	left, err := g.r.familyLeft(t.Context(), p)
	if err != nil {
		t.Fatal(err)
	}
	g.reconcile(t, child(2), 1)
	got := childTask(t, g, 2).Spec.Budget
	if want := int64(float64(left) / 1.70); got.TaskTokens != want || got.TaskTokens >= 3_000_000 || got.TaskMicroUSD != left {
		t.Fatalf("tokens %d want %d (left %d), usd %d", got.TaskTokens, want, left, got.TaskMicroUSD)
	}
}

// Offline proof 4, SR10: Create of the part's Task fails once after its issue was opened; the
// replay adopts the issue by its marker and ends with one issue and one Task.
func TestAFailedCreateReplaysToOneIssueAndOneTask(t *testing.T) {
	g := proposed(t)
	fail := true
	c := interceptor.NewClient(g.c.(client.WithWatch), interceptor.Funcs{Create: func(ctx context.Context, cl client.WithWatch, o client.Object, opts ...client.CreateOption) error {
		if tk, ok := o.(*v1alpha1.Task); ok && tk.Spec.Parent != nil && fail {
			fail = false
			return errors.New("apiserver blip")
		}
		return cl.Create(ctx, o, opts...)
	}})
	g.r.Client = c
	g.r.Now = func() time.Time { return now.Add(time.Hour) }
	g.f.SetComments(7, splitBy(81, "Smana"))
	req := ctrl.Request{NamespacedName: taskKey("3buqdlot")}
	_, _ = g.r.Reconcile(t.Context(), req) // Escalated → Split
	if _, err := g.r.Reconcile(t.Context(), req); err == nil {
		t.Fatal("the Task create fails once")
	}
	if _, err := g.r.Reconcile(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	var l v1alpha1.TaskList
	_ = g.c.List(t.Context(), &l)
	if len(g.f.Created()) != 1 || len(l.Items) != 2 {
		t.Fatalf("issues %q tasks %d", g.f.Created(), len(l.Items))
	}
}

// openRevertRig is an approved split whose part 1 auto-merged as PR 12, ready to be reverted.
func openRevertRig(t *testing.T) (*rig, *v1alpha1.Task) {
	t.Helper()
	g := approved(t)
	g.r.Merger = g.f
	g.r.Cfg.Classes = map[string]config.Class{"revert": {Live: true}}
	g.f.SetPR(forge.PR{Number: 12, NodeID: "PR_12", State: "MERGED", Title: "docs: part 1"})
	c := childTask(t, g, 1)
	merged := metav1.NewTime(now)
	c.Status.PullRequest = &v1alpha1.PullRequestRef{Number: 12, NodeID: "PR_12", AutoMerged: true, MergedAt: &merged}
	return g, c
}

func toParent(c *v1alpha1.Task) bool {
	return slices.ContainsFunc(c.Status.Outbox, func(o v1alpha1.Narration) bool { return o.Number == 7 && strings.Contains(o.Body, "#901") })
}

// SP6, through revert(): a part reverted after its parent closed comments on the parent issue;
// before the close, nothing extra.
func TestARevertedPartCommentsOnAClosedParent(t *testing.T) {
	g, c := openRevertRig(t)
	if err := g.r.revert(t.Context(), c, "main_red"); err != nil || toParent(c) {
		t.Fatalf("before the close nothing extra: %+v %v", c.Status.Outbox, err)
	}
	g, c = openRevertRig(t)
	setPhase(t, g, "3buqdlot", v1alpha1.PhaseDone, "children_merged")
	if err := g.r.revert(t.Context(), c, "main_red"); err != nil || !toParent(c) {
		t.Fatalf("%+v %v", c.Status.Outbox, err)
	}
}

// A part reverted while its parent waits Escalated on budget-family is told to the parent's issue,
// and an expiring parent does not count that part as landed.
func TestARevertedPartIsNotCountedLandedByAnExpiringParent(t *testing.T) {
	g, c := openRevertRig(t)
	var p v1alpha1.Task
	_ = g.c.Get(t.Context(), taskKey("3buqdlot"), &p)
	p.Status.Phase, p.Status.Reason = v1alpha1.PhaseEscalated, "budget-family"
	p.Status.Split.Children[0].Outcome = "landed"
	if err := g.c.Status().Update(t.Context(), &p); err != nil {
		t.Fatal(err)
	}
	if err := g.r.revert(t.Context(), c, "main_red"); err != nil || !toParent(c) {
		t.Fatalf("%+v %v", c.Status.Outbox, err)
	}
	_ = g.c.Get(t.Context(), taskKey("3buqdlot"), &p)
	if got := p.Status.Split.Children[0].Outcome; got != "reverted" {
		t.Fatalf("outcome %q", got)
	}
	g.r.countFamily(t.Context(), &p)
	if slices.Contains(g.metrics.recorded, "split_child review landed") || !slices.Contains(g.metrics.recorded, "split_child review reverted") {
		t.Fatalf("%v", g.metrics.recorded)
	}
}

// A part's issue names its real branch, not a placeholder.
func TestAPartsIssueNamesItsBranch(t *testing.T) {
	g := approved(t)
	if body := g.f.Created()[0]; !strings.Contains(body, "`agent/"+child(1)+"`") || strings.Contains(body, "<task>") {
		t.Fatalf("%q", body)
	}
}

// A tie in creation time breaks on the name, the same way both ways round.
func TestNewerBreaksTiesByName(t *testing.T) {
	ts := metav1.NewTime(now)
	a := &v1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "aaa", CreationTimestamp: ts}}
	b := &v1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "bbb", CreationTimestamp: ts}}
	if newer(a, b) == newer(b, a) {
		t.Fatal("a tie must have one winner")
	}
}

// SL12: an escalated part keeps the parent waiting while its issue is open.
func TestAnEscalatedPartKeepsTheParentWaiting(t *testing.T) {
	g := approved(t)
	setPhase(t, g, child(1), v1alpha1.PhaseEscalated, "ci_red")
	if p := g.reconcile(t, "3buqdlot", 1); p.Status.Phase != v1alpha1.PhaseSplit || len(g.f.Created()) != 1 {
		t.Fatalf("%s %q", p.Status.Phase, g.f.Created())
	}
}

// SL10, offline proofs 5 and 8: a family that cannot hold one more run ends Escalated, budget-family,
// and a retry never runs the parent whole; the family's selector sums its spend.
func TestTheFamilysCapHolds(t *testing.T) {
	g := approved(t)
	var c v1alpha1.Task
	_ = g.c.Get(t.Context(), taskKey(child(1)), &c)
	c.Status.Phase, c.Status.Reason = v1alpha1.PhaseDone, "merged"
	c.Status.Runs = []v1alpha1.RunRecord{{ID: runID(child(1), 0), Role: "implementer", Trigger: "initial", CostMicroUSD: 18_500_000, Tokens: 1}}
	_ = g.c.Status().Update(t.Context(), &c)
	p := g.reconcile(t, "3buqdlot", 1)
	if p.Status.Phase != v1alpha1.PhaseEscalated || p.Status.Reason != "budget-family" || len(g.f.Created()) != 1 {
		t.Fatalf("%s %s %q", p.Status.Phase, p.Status.Reason, g.f.Created())
	}
	left, err := g.r.familyLeft(t.Context(), p)
	if err != nil || left != p.Spec.Split.FamilyMicroUSD-spend(p)-18_500_000 {
		t.Fatalf("left %d (%v)", left, err)
	}
	g.r.Now = func() time.Time { return now.Add(2 * time.Hour) }
	g.f.SetComments(7, retryBy(90, "Smana", -90*time.Minute))
	if p = g.reconcile(t, "3buqdlot", 1); p.Status.Phase != v1alpha1.PhaseEscalated {
		t.Fatalf("a retry ran an approved parent (SR9): %s", p.Status.Phase)
	}
}
