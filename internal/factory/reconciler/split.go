// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/factory/api"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/fmetrics"
	"github.com/Smana/agent-platform/internal/factory/forge"
	"github.com/Smana/agent-platform/internal/factory/narrate"
	"github.com/Smana/agent-platform/internal/factory/taskid"
	"github.com/Smana/agent-platform/internal/factory/triage"
)

// splitAtTriage offers the scoper a split (splitter design SL3, SP1, plan SR5): a task with an issue,
// in mode scoper, whose class's row resolves a split, on C7's own frontier verdict —
// status.classification.tier, which the control group never changes (OD-14). Recorded at triage, so
// a config change never moves a task already triaged.
func (r *Reconciler) splitAtTriage(t *v1alpha1.Task, cl v1alpha1.Classification) {
	t.Spec.Split = nil
	if t.Spec.Issue == 0 || t.Spec.ScopeMode != string(config.ScopeScoper) || cl.Tier != "frontier" {
		return
	}
	if p := r.Cfg.SplitFor(t.Spec.Repository, t.Spec.PredictedClass); p != nil {
		t.Spec.Split = &v1alpha1.SplitSpec{MaxChildren: p.MaxChildren, FamilyMicroUSD: config.MicroUSD(p.FamilyUSD), ChildTier: p.ChildTier}
	}
}

// The split's reasons, label and verb (splitter design SL4, SL10, SL12; plan SR18).
const (
	ReasonSplitProposed  = "split_proposed"
	ReasonChildrenMerged = "children_merged"
	ReasonChildDropped   = "child_dropped"
	ReasonBudgetFamily   = "budget-family"
	LabelSplitProposed   = "factory/split-proposed"
	cmdSplit             = "/factory split"
)

// A split's states on its parent (SL14), and a child's outcomes.
const (
	SplitProposed   = "proposed"
	SplitApproved   = "approved"
	SplitDeclined   = "declined"
	SplitSuperseded = "superseded"
	SplitExpired    = "expired"
	SplitIgnored    = "ignored"
	ChildLanded     = "landed"
	ChildDropped    = "dropped"
	ChildReverted   = "reverted"
)

// splitHandoff is the scoper's second answer (SL4, SR6): a split the task was offered goes to a
// maintainer; any other is ignored and counted, and the task runs on its scope.
func (r *Reconciler) splitHandoff(ctx context.Context, t *v1alpha1.Task, sp *envelope.Split, seq int64) error {
	if t.Spec.Split == nil || len(sp.Children) > int(t.Spec.Split.MaxChildren) || sp.Validate() != nil {
		r.ignoreSplit(ctx, t, seq)
		return r.scopeEnd(ctx, t, "ready", seq)
	}
	return r.proposeSplit(ctx, t, sp, seq)
}

// ignoreSplit records and counts a split the factory will not propose (SL14).
func (r *Reconciler) ignoreSplit(ctx context.Context, t *v1alpha1.Task, seq int64) {
	t.Status.Split = &v1alpha1.SplitStatus{State: SplitIgnored, Seq: seq}
	class := t.Spec.PredictedClass
	record(ctx, func(ctx context.Context) { r.Metrics.Split(ctx, class, SplitIgnored) })
}

// proposeSplit is SL4 (SR7): Escalated, split_proposed, one comment and the label, no child. Not
// end(), as bounce is not: the comment is the plan, and a proposal says nothing of the tier. The
// whole task's scope is recorded ready, so a /factory retry runs it whole with no second scoper run.
func (r *Reconciler) proposeSplit(ctx context.Context, t *v1alpha1.Task, sp *envelope.Split, seq int64) error {
	t.Status.Scope = &v1alpha1.ScopeStatus{State: "ready", Seq: seq}
	t.Status.Split = &v1alpha1.SplitStatus{State: SplitProposed, Seq: seq, Of: int32(len(sp.Children))} //nolint:gosec // Validate bounds it to 2..5
	class, n, name := t.Spec.PredictedClass, t.Spec.Issue, t.Name
	record(ctx, func(ctx context.Context) { r.Metrics.ScoperRun(ctx, class, "ready") })
	record(ctx, func(ctx context.Context) {
		if err := r.gh(ctx).AddLabels(ctx, n, LabelSplitProposed); err != nil {
			r.log().Warn("label not added", "task.id", name, "label", LabelSplitProposed, "err", err)
		}
	})
	tasks := make([]string, 0, len(sp.Children))
	for _, c := range sp.Children {
		tasks = append(tasks, c.Task)
	}
	r.releaseUnheld(ctx, t)
	r.to(t, v1alpha1.PhaseEscalated, ReasonSplitProposed)
	narrateLater(t, narrate.SplitProposed(t, sp.Why, tasks, r.repo(t).Maintainers, r.Cfg.RoomsURL))
	r.outcome(ctx, t)
	return nil
}

// escalatedSplit is the escalated wait of a task with a split (SL4, SR8, SR9). A proposed split
// waits for /factory split, read before /factory retry. An approved parent, escalated on its
// family's budget, never runs an implementer: only its expiry ends it. done is false when escalated
// goes on as for any task.
func (r *Reconciler) escalatedSplit(ctx context.Context, t *v1alpha1.Task, pr forge.PR) (bool, error) {
	sp := t.Status.Split
	switch {
	case sp == nil:
		return false, nil
	case sp.State == SplitApproved:
		return true, r.expireEscalation(ctx, t)
	case sp.State != SplitProposed || t.Status.Reason != ReasonSplitProposed:
		return false, nil
	}
	c, ok, err := r.command(ctx, t, pr, cmdSplit)
	if err != nil || !ok {
		return false, err
	}
	return true, r.approveSplit(ctx, t, c)
}

// approveSplit is SL4 (a) and SL11: the approver pays for every part, so their day is checked first,
// as intake checks a labeller's (D4). The command is handled only once the ledger has answered, in
// the same status write as the outcome: a ledger read that fails leaves it for the next pass. A
// spent day is said once and the split keeps waiting; the approver comments `/factory split` again
// tomorrow. Approved, the parent waits in Split, and its next pass opens part 1.
func (r *Reconciler) approveSplit(ctx context.Context, t *v1alpha1.Task, c forge.Comment) error {
	person := v1alpha1.Person(c.Author)
	spent, err := r.daySpent(ctx, t, person)
	if err != nil {
		return err
	}
	markHandled(t, c.ID)
	if spent {
		narrateLater(t, narrate.ApproverOverBudget(t.Spec.Issue, person, r.Cfg.Budgets.DailyMicroUSD(person), c.At))
		record(ctx, func(ctx context.Context) {
			r.Metrics.AdmissionRefused(fmetrics.WithPrincipal(ctx, person), "person_budget_exhausted") // the approver (SI4)
		})
		return nil
	}
	r.leaveSplit(ctx, t, SplitApproved)
	t.Status.Split.ApprovedBy = person
	record(ctx, func(ctx context.Context) { r.Metrics.Intervention(ctx, "split") })
	r.to(t, v1alpha1.PhaseSplit, "")
	return nil
}

// daySpent is D4 for an approver: with per-person caps on, person's day already spent. Enforced it
// refuses; in shadow it only logs. Queued's reservation per run stays the exact check (D3).
func (r *Reconciler) daySpent(ctx context.Context, t *v1alpha1.Task, person string) (bool, error) {
	b := r.Cfg.Budgets
	if !b.PerPerson.Enabled || r.Ledger == nil {
		return false, nil
	}
	used, err := api.DayUsed(ctx, r.Ledger, r.Namespace, person, r.Now())
	if err != nil {
		return false, err // no answer is no refusal: the command waits for the next pass
	}
	day := b.DailyMicroUSD(person)
	if used < day {
		return false, nil
	}
	if !b.EnforcePrincipal {
		r.log().Info("approver's day is spent (shadow)", "task.id", t.Name, "person", person, "used_micro_usd", used, "cap_micro_usd", day)
		return false, nil
	}
	return true, nil
}

// leaveSplit is a split_proposed task leaving its wait (SL4, SL14): the label goes, and the split's
// state is set and counted once. Called before the transition, while the reason still says so.
func (r *Reconciler) leaveSplit(ctx context.Context, t *v1alpha1.Task, state string) {
	sp := t.Status.Split
	if sp == nil || sp.State != SplitProposed || t.Status.Phase != v1alpha1.PhaseEscalated || t.Status.Reason != ReasonSplitProposed {
		return
	}
	sp.State = state
	class, n, name := t.Spec.PredictedClass, t.Spec.Issue, t.Name
	record(ctx, func(ctx context.Context) { r.Metrics.Split(ctx, class, state) })
	record(ctx, func(ctx context.Context) {
		if err := r.gh(ctx).RemoveLabel(ctx, n, LabelSplitProposed); err != nil {
			r.log().Warn("label not removed", "task.id", name, "label", LabelSplitProposed, "err", err)
		}
	})
}

// family is the parent's children (SL14, SR4): the newest generation of each part by index, and
// every generation for the family's spend, read through the label the factory and the intake set.
func (r *Reconciler) family(ctx context.Context, p *v1alpha1.Task) (map[int32]*v1alpha1.Task, []v1alpha1.Task, error) {
	var l v1alpha1.TaskList
	if err := r.Client.List(ctx, &l, client.InNamespace(r.Namespace), client.MatchingLabels{v1alpha1.LabelParent: p.Name}); err != nil {
		return nil, nil, err
	}
	newest := map[int32]*v1alpha1.Task{}
	for i := range l.Items {
		c := &l.Items[i]
		if c.Spec.Parent == nil || c.Spec.Parent.Task != p.Name {
			continue // a label anyone could set names no family; the spec, which CEL fixes, does
		}
		if o, ok := newest[c.Spec.Parent.Index]; !ok || newer(c, o) {
			newest[c.Spec.Parent.Index] = c
		}
	}
	return newest, l.Items, nil
}

// newer: a's generation follows b's (SR13). The API server stamps creation to the second; on a tie,
// a re-labelled generation (an issue key) follows the factory's own (a split key).
func newer(a, b *v1alpha1.Task) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return b.CreationTimestamp.Before(&a.CreationTimestamp)
	}
	return strings.HasPrefix(a.Spec.Source.Key, "github:issue:") && !strings.HasPrefix(b.Spec.Source.Key, "github:issue:")
}

// spend is what a task's runs cost, a run the meter could not price at the reference rate the day
// ledger charges it (TC7): a family's cap never counts an unpriced run as $0.
func spend(t *v1alpha1.Task) int64 {
	var sum int64
	for _, rec := range t.Status.Runs {
		if rec.Unpriced != "" || (rec.Tokens > 0 && rec.CostMicroUSD == 0) {
			sum += config.RefMicroUSD(rec.Tokens)
		} else {
			sum += rec.CostMicroUSD
		}
	}
	return sum
}

// familyLeft is SL10: the family's cap less what the parent and every child, of every generation,
// spent. Read when a part opens and at its triage: parts run one at a time, so nothing else spends.
func (r *Reconciler) familyLeft(ctx context.Context, p *v1alpha1.Task) (int64, error) {
	_, all, err := r.family(ctx, p)
	if err != nil {
		return 0, err
	}
	left := p.Spec.Split.FamilyMicroUSD - spend(p)
	for i := range all {
		left -= spend(&all[i])
	}
	return left, nil
}

// childBudget is a part's caps (SL9, SL10): its tier's, its task cap no more than the family has left.
func (r *Reconciler) childBudget(p *v1alpha1.Task, left int64) v1alpha1.Budget {
	b := triage.BudgetFor(r.Cfg, p.Spec.Split.ChildTier)
	b.TaskMicroUSD = min(b.TaskMicroUSD, left)
	return b
}

// fits: the cap holds one whole run (runFits' rule, owner 2026-10-07).
func fits(b v1alpha1.Budget) bool { return b.TaskMicroUSD > 0 && b.TaskMicroUSD >= b.RunMicroUSD }

// childScope is part k of the parent's split: the handoff status.split names, checked again.
func (r *Reconciler) childScope(ctx context.Context, p *v1alpha1.Task, k int32) (*envelope.Scope, error) {
	seq := p.Status.Split.Seq
	evs, _, err := r.Rooms.EventsSince(ctx, p.Status.RoomRef, seq-1, func(e envelope.Event) bool {
		return e.Seq == seq && e.Type == envelope.Handoff
	})
	if err != nil {
		return nil, err
	}
	var h envelope.HandoffPayload
	if len(evs) == 0 || json.Unmarshal(evs[0].Payload, &h) != nil || h.FromRole != "triager" || h.Split == nil ||
		h.Split.Validate() != nil || int(k) > len(h.Split.Children) {
		return nil, fmt.Errorf("task %s: the handoff at seq %d holds no part %d", p.Name, seq, k)
	}
	return &h.Split.Children[k-1], nil
}

// taskScopeOf is a part's scope as the Task keeps it.
func taskScopeOf(s *envelope.Scope) *v1alpha1.TaskScope {
	out := &v1alpha1.TaskScope{State: s.State, Task: s.Task, Touches: s.Touches, Leave: s.Leave, Plan: s.Plan, Assumptions: s.Assumptions}
	for _, a := range s.Acceptance {
		out.Acceptance = append(out.Acceptance, v1alpha1.TaskAcceptance{Check: a.Check, Proves: a.Proves})
	}
	return out
}

// openChild opens part k (SL7, SL8, SR10): its issue, found by its marker or created, and its Task,
// recorded on the parent in this pass. Each step is idempotent, so a replay after a lost status
// write opens no second issue and creates no second Task.
func (r *Reconciler) openChild(ctx context.Context, t *v1alpha1.Task, k int32) error {
	sp := t.Status.Split
	sc, err := r.childScope(ctx, t, k)
	if err != nil {
		return err
	}
	iss, err := r.childIssue(ctx, t, k, sc)
	if err != nil {
		return err
	}
	key := taskid.SplitKey(t.Name, int(k))
	name := taskid.Name(key)
	c := &v1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.Namespace,
			Labels: map[string]string{v1alpha1.LabelIssue: strconv.Itoa(iss.Number), v1alpha1.LabelParent: t.Name}},
		Spec: v1alpha1.TaskSpec{
			Source: v1alpha1.Source{Kind: "issue", Ref: fmt.Sprintf("%s#%d", t.Spec.Repository, iss.Number), Key: key,
				RequestedBy: sp.ApprovedBy, Trust: t.Spec.Source.Trust, ContentSHA256: t.Spec.Source.ContentSHA256},
			Repository: t.Spec.Repository, Issue: iss.Number, IssueAuthor: t.Spec.IssueAuthor, Text: t.Spec.Text, DataClass: t.Spec.DataClass,
			Parent: &v1alpha1.Parent{Task: t.Name, Issue: t.Spec.Issue, Index: k, Of: sp.Of},
			Scope:  taskScopeOf(sc),
		},
	}
	if err := r.Client.Create(ctx, c); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("open part %d of %s: %w", k, t.Name, err)
	}
	if !slices.ContainsFunc(sp.Children, func(c v1alpha1.SplitChild) bool { return c.Index == k }) {
		sp.Children = append(sp.Children, v1alpha1.SplitChild{Index: k, Issue: iss.Number, Task: name})
	}
	narrateLater(t, narrate.SplitPart(t, k, iss.Number))
	return nil
}

// childIssue is part k's issue (SL8): the one the factory opened already, by its marker among the
// issues labelled child-of the parent, open or closed; else a new one, whose sub-issue link is a
// view for people and never blocks.
func (r *Reconciler) childIssue(ctx context.Context, t *v1alpha1.Task, k int32, sc *envelope.Scope) (forge.Issue, error) {
	label, marker := v1alpha1.ChildOfLabel(t.Spec.Issue), narrate.SplitMarker(t.Name, int(k))
	found, err := r.gh(ctx).IssuesLabelled(ctx, label)
	if err != nil {
		return forge.Issue{}, err
	}
	for _, i := range found {
		if forge.SameLogin(i.Author, r.Cfg.FactoryLogin) && strings.Contains(i.Body, marker) {
			return i, nil // a replay: the issue exists, its record was lost
		}
	}
	labels := []string{label}
	if c := t.Spec.PredictedClass; c != "" && c != "review" {
		labels = append(labels, "class:"+c) // the parent's class (SL6), applied by the factory: never read as a prediction (T3)
	}
	title, body := narrate.ChildIssue(t, k, sc, r.Cfg.RoomsURL)
	iss, err := r.gh(ctx).CreateIssue(ctx, title, body, labels)
	if err != nil {
		return forge.Issue{}, err
	}
	if err := r.gh(ctx).AddSubIssue(ctx, t.Spec.Issue, iss.ID); err != nil {
		r.log().Warn("sub-issue link not added", "task.id", t.Name, "issue", iss.Number, "err", err)
	}
	return iss, nil
}

// receivedChild is a part's triage (SL5, SL9, SL10, SR11): its parent's spec but for its part, its
// tier and its cap; C7 is never asked. Every generation passes here, the factory's and a re-labelled
// one, so they are built alike.
func (r *Reconciler) receivedChild(ctx context.Context, t *v1alpha1.Task) error {
	reason, err := r.inherit(ctx, t)
	if err != nil {
		return err
	}
	if reason != "" {
		return r.end(ctx, t, v1alpha1.PhaseRejected, reason)
	}
	status := t.Status // Update returns the server's status into t; keep ours
	if err := r.Client.Update(ctx, t); err != nil {
		return err
	}
	t.Status = status
	t.Status.Classification = &v1alpha1.Classification{Tier: t.Spec.Budget.Tier, Classifier: "split", Fallback: "static"}
	t.Status.ConfigHash = r.Cfg.Hash
	r.to(t, v1alpha1.PhaseTriaged, "")
	return nil
}

// inherit fills a part's spec from its parent; reason is why it cannot run.
func (r *Reconciler) inherit(ctx context.Context, t *v1alpha1.Task) (string, error) {
	var p v1alpha1.Task
	switch err := r.Client.Get(ctx, types.NamespacedName{Namespace: r.Namespace, Name: t.Spec.Parent.Task}, &p); {
	case apierrors.IsNotFound(err):
		return "parent_missing", nil
	case err != nil:
		return "", err
	}
	if p.Status.Phase != v1alpha1.PhaseSplit || p.Spec.Split == nil || p.Status.Split == nil {
		return "parent_not_split", nil
	}
	left, err := r.familyLeft(ctx, &p)
	if err != nil {
		return "", err
	}
	b := r.childBudget(&p, left)
	if !fits(b) {
		return ReasonBudgetFamily, nil
	}
	if t.Spec.Scope == nil { // a re-labelled generation: the part's scope from the parent's split
		sc, err := r.childScope(ctx, &p, t.Spec.Parent.Index)
		if err != nil {
			return "", err
		}
		t.Spec.Scope = taskScopeOf(sc)
	}
	// SC9: the snapshot a maintainer labelled wins over an agent's reading, so a part keeps it whole.
	t.Spec.Text, t.Spec.Source.ContentSHA256 = p.Spec.Text, p.Spec.Source.ContentSHA256
	t.Spec.PredictedClass, t.Spec.DataClass = p.Spec.PredictedClass, p.Spec.DataClass // SL6: the parent's class
	t.Spec.Steering, t.Spec.ScopeMode, t.Spec.Loop = p.Spec.Steering, p.Spec.ScopeMode, p.Spec.Loop
	t.Spec.Template, t.Spec.Budget = triage.Matrix(p.Spec.PredictedClass, "issue", b.Tier), b
	if t.Labels == nil {
		t.Labels = map[string]string{}
	}
	t.Labels[v1alpha1.LabelParent] = p.Name
	return "", nil
}

// splitting is the parent's wait in Split (SL7, SL12): it opens the next part once the one before
// has landed. Task 10 adds the outcomes and the end.
func (r *Reconciler) splitting(ctx context.Context, t *v1alpha1.Task) error {
	if len(t.Status.Split.Children) == 0 {
		return r.openChild(ctx, t, 1)
	}
	return nil
}
