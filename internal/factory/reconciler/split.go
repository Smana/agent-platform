// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/factory/api"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/fmetrics"
	"github.com/Smana/agent-platform/internal/factory/forge"
	"github.com/Smana/agent-platform/internal/factory/narrate"
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

// splitting is the parent's wait in Split (SL7, SL12): Task 9 fills it.
func (r *Reconciler) splitting(context.Context, *v1alpha1.Task) error { return nil }
