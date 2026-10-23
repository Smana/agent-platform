// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"encoding/json"
	"slices"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/narrate"
	"github.com/Smana/agent-platform/internal/factory/runs"
)

// scoperDue: the task's mode is scoper and its scoper has not ended (scoper plan S13). A resumed or
// retried scoper is due again; once status.scope is set, every later run is an implementer's.
func scoperDue(t *v1alpha1.Task) bool {
	return t.Spec.ScopeMode == string(config.ScopeScoper) && t.Status.Scope == nil
}

// scoperSpec is the scoper's claim: a triager run at the light tier's run caps whatever the task's
// tier (SO2), on the task's model until per-role models exist (SC10). The tier label is what the
// meter caps the run's USD by. Its spend counts toward the task's caps as every run's does.
func (r *Reconciler) scoperSpec(t *v1alpha1.Task, text string) runs.Spec {
	s := r.implementerSpec(t, text)
	light := r.Cfg.Tiers["light"]
	s.Role, s.Tier, s.MaxTokens, s.MaxMinutes = "triager", "light", light.RunTokens, light.RunMinutes
	return s
}

// startScoper starts the scoper run, or resumes a lost one with its own brief.
func (r *Reconciler) startScoper(ctx context.Context, t *v1alpha1.Task) error {
	text, ok, err := r.resumeBrief(ctx, t)
	if err != nil {
		return err
	}
	if !ok {
		text = ScoperBrief(t, r.Nonce())
	}
	return r.startRun(ctx, t, r.scoperSpec(t, text), nextTrigger(t))
}

// The bounce's reason and label (SC8). The label is an output the factory never reads.
const (
	ReasonNeedsAuthor = "needs_author"
	LabelNeedsAuthor  = "factory/needs-author"
)

// escalatesScoper are the scoper ends that escalate as any run's (scoper plan S14): a person
// stopped it, or the task, its payer or the fleet has no budget left, or the cluster had no
// capacity (run_unschedulable) or the task no budget (resume_budget) for it: capacity and budget
// ends, not the scoper's failure. Every other end fails open.
var escalatesScoper = []string{"manual", "revoked", "deleted", "budget-task", "budget-principal", "budget-fleet", "budget-person",
	"run_unschedulable", "resume_budget"}

// scoped ends the scoper run (SC8, SC11): the implementer is queued with the scope, or without it
// when the run left none; a question bounces the task to its issue's author.
func (r *Reconciler) scoped(ctx context.Context, t *v1alpha1.Task, phase, reason string) error {
	if slices.Contains(escalatesScoper, reason) {
		return r.end(ctx, t, v1alpha1.PhaseEscalated, reason)
	}
	sc, seq, err := r.lastScope(ctx, t)
	if err != nil {
		return err
	}
	switch {
	case sc == nil && phase == "Succeeded":
		return r.scopeEnd(ctx, t, "missing", 0)
	case sc == nil:
		return r.scopeEnd(ctx, t, "failed", 0)
	case sc.State == envelope.ScopeReady:
		return r.scopeEnd(ctx, t, "ready", seq)
	case t.Spec.Issue == 0: // a schedule has no author to ask
		return r.scopeEnd(ctx, t, "assumed", seq)
	}
	return r.bounce(ctx, t, sc, seq)
}

// lastScope is the scoper run's scope: the last handoff of the current run that carries a valid
// one, after the run's start seq, and that handoff's seq. The factory checks it again: the broker
// stored it redacted.
func (r *Reconciler) lastScope(ctx context.Context, t *v1alpha1.Task) (*envelope.Scope, int64, error) {
	cur := current(t)
	evs, _, err := r.roomTail(ctx, t.Status.RoomRef, cur.StartSeq, func(e envelope.Event) bool {
		return e.Type == envelope.Handoff && e.RunID == cur.ID
	})
	if err != nil {
		return nil, 0, err
	}
	for i := len(evs) - 1; i >= 0; i-- {
		var p envelope.HandoffPayload
		// Only the handoff to the implementer carries a scope the factory uses.
		if json.Unmarshal(evs[i].Payload, &p) == nil && p.ToRole == "implementer" && p.Scope != nil && p.Scope.Validate() == nil {
			return p.Scope, evs[i].Seq, nil
		}
	}
	return nil, 0, nil
}

// scopeEnd records the scoper's outcome and queues the implementer's first run.
func (r *Reconciler) scopeEnd(ctx context.Context, t *v1alpha1.Task, state string, seq int64) error {
	t.Status.Scope = &v1alpha1.ScopeStatus{State: state, Seq: seq}
	class := t.Spec.PredictedClass
	record(ctx, func(ctx context.Context) { r.Metrics.ScoperRun(ctx, class, state) })
	t.Status.NextTrigger = "initial" // the scoper's run was no attempt at the work
	r.to(t, v1alpha1.PhaseQueued, "")
	return nil
}

// bounce is SC8: Escalated, needs_author, one comment and the label. Not end(): its Ended comment
// would be a second one, and a question says nothing of the tier.
func (r *Reconciler) bounce(ctx context.Context, t *v1alpha1.Task, sc *envelope.Scope, seq int64) error {
	t.Status.Scope = &v1alpha1.ScopeStatus{State: "question", Seq: seq}
	class, n, name := t.Spec.PredictedClass, t.Spec.Issue, t.Name
	record(ctx, func(ctx context.Context) { r.Metrics.ScoperRun(ctx, class, "question") })
	record(ctx, func(ctx context.Context) {
		if err := r.gh(ctx).AddLabels(ctx, n, LabelNeedsAuthor); err != nil {
			r.log().Warn("label not added", "task.id", name, "label", LabelNeedsAuthor, "err", err)
		}
	})
	r.releaseUnheld(ctx, t)
	r.to(t, v1alpha1.PhaseEscalated, ReasonNeedsAuthor)
	narrateLater(t, narrate.NeedsAuthor(t, sc.Question, sc.Assumptions[0], r.repo(t).Maintainers, r.Cfg.RoomsURL))
	r.outcome(ctx, t)
	return nil
}

// scoperRun: the task's current run is its scoper's, not yet ended into status.scope. An investigate
// task is never in scoper mode, so R38's end is untouched.
func scoperRun(t *v1alpha1.Task) bool {
	return scoperDue(t) && len(t.Status.Runs) > 0 && current(t).Role == "triager"
}

// taskScope is the task's scope for an implementer brief: the handoff status.scope names, nil when
// the scoper left none or never ran.
func (r *Reconciler) taskScope(ctx context.Context, t *v1alpha1.Task) (*Scoped, error) {
	st := t.Status.Scope
	if st == nil || st.Seq == 0 {
		return nil, nil
	}
	evs, _, err := r.Rooms.EventsSince(ctx, t.Status.RoomRef, st.Seq-1, func(e envelope.Event) bool {
		return e.Seq == st.Seq && e.Type == envelope.Handoff
	})
	if err != nil || len(evs) == 0 {
		return nil, err
	}
	var p envelope.HandoffPayload
	// Checked again: the room is the broker's, but this text reaches a prompt.
	if json.Unmarshal(evs[0].Payload, &p) != nil || p.Scope == nil || p.FromRole != "triager" || p.ToRole != "implementer" || p.Scope.Validate() != nil {
		return nil, nil
	}
	return &Scoped{Scope: *p.Scope, Seq: st.Seq, Assumed: st.State == "assumed"}, nil
}

// leaveBounce is a needs_author task leaving Escalated (SC8, SC12): the label goes, and exit, when
// one of the three, is counted (best effort: a replayed reconcile can count an exit twice). Called before the transition, while the reason still says so.
func (r *Reconciler) leaveBounce(ctx context.Context, t *v1alpha1.Task, exit string) {
	if t.Status.Phase != v1alpha1.PhaseEscalated || t.Status.Reason != ReasonNeedsAuthor {
		return
	}
	class, n, name := t.Spec.PredictedClass, t.Spec.Issue, t.Name
	if exit != "" {
		record(ctx, func(ctx context.Context) { r.Metrics.ScopeBounce(ctx, class, exit) })
	}
	record(ctx, func(ctx context.Context) {
		if err := r.gh(ctx).RemoveLabel(ctx, n, LabelNeedsAuthor); err != nil {
			r.log().Warn("label not removed", "task.id", name, "label", LabelNeedsAuthor, "err", err)
		}
	})
}
