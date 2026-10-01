// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	roomv1 "github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/factory/forge"
	"github.com/Smana/agent-platform/internal/factory/narrate"
	"github.com/Smana/agent-platform/internal/factory/rooms"
	"github.com/Smana/agent-platform/internal/factory/runs"
	"github.com/Smana/agent-platform/internal/factory/tracing"
)

// runEndGrace is how long a finished run waits for the room's end reason before falling back to
// the AgentRun's phase: SP2 knows why a run ended, the AgentRun only says Failed.
const runEndGrace = time.Minute

// slotFree: the active-task and concurrent-run caps (§6.2), and never while a human drives (C4).
func (r *Reconciler) slotFree(ctx context.Context, t *v1alpha1.Task) (bool, string, error) {
	active, err := r.countTasks(ctx, func(o *v1alpha1.Task) bool { return o.Name != t.Name && v1alpha1.ActivePhase(o.Status.Phase) })
	if err != nil {
		return false, "", err
	}
	if active >= r.Cfg.Caps.ActiveTasks {
		return false, "waiting_active_tasks", nil
	}
	all, err := r.Runs.List(ctx)
	if err != nil {
		return false, "", err
	}
	live := 0
	for _, x := range all {
		if runs.Terminal(x.Phase) {
			continue
		}
		if x.Principal == runs.PrincipalFactory {
			live++
		}
		// One live run per room (C3: a room's runs share its branch). The task's own was adopted
		// before; anyone else's, a run a human started in the room, is waited for.
		if x.RoomRef == t.Status.RoomRef && x.TaskID != t.Name {
			return false, "waiting_room_busy", nil
		}
	}
	if live >= r.Cfg.Caps.ConcurrentRuns {
		return false, "waiting_run_slot", nil
	}
	var room roomv1.Room
	if err := r.Client.Get(ctx, types.NamespacedName{Namespace: r.Namespace, Name: t.Status.RoomRef}, &room); err != nil {
		return false, "", err
	}
	if rooms.HumanDriver(&room) {
		return false, "waiting_human_driver", nil
	}
	return true, "", nil
}

// adopt records a live run of this task that status does not know about: the run was created
// but the status write that recorded it was lost. It is never duplicated.
func (r *Reconciler) adopt(ctx context.Context, t *v1alpha1.Task) (bool, error) {
	all, err := r.Runs.List(ctx)
	if err != nil {
		return false, err
	}
	for _, x := range all {
		if x.TaskID != t.Name || runs.Terminal(x.Phase) || known(t, x.ID) {
			continue
		}
		now := metav1.NewTime(r.Now())
		t.Status.Runs = append(t.Status.Runs, v1alpha1.RunRecord{ID: x.ID, Role: x.Role, Trigger: nextTrigger(t), Started: &now})
		t.Status.NextTrigger = ""
		r.to(t, phaseFor(x.Role), "adopted")
		return true, nil
	}
	return false, nil
}

func known(t *v1alpha1.Task, id string) bool {
	for _, rec := range t.Status.Runs {
		if rec.ID == id {
			return true
		}
	}
	return false
}

func phaseFor(role string) string {
	if role == "reviewer" || role == "tester" {
		return v1alpha1.PhaseReviewing
	}
	return v1alpha1.PhaseImplementing
}

// sourceURL is the issue a task narrates on, the harness footer's Agent-Task (ruling SF).
func sourceURL(t *v1alpha1.Task) string {
	if t.Spec.Issue == 0 {
		return ""
	}
	return fmt.Sprintf("https://github.com/%s/issues/%d", t.Spec.Repository, t.Spec.Issue)
}

// implementerSpec: every run of a task writes agent/<taskId> (C3); baseRef main, because the
// harness resumes origin/<branch> when an earlier run pushed it (SP1 R7).
func (r *Reconciler) implementerSpec(t *v1alpha1.Task, text string) runs.Spec {
	return runs.Spec{TaskID: t.Name, Role: "implementer", Repository: t.Spec.Repository, BaseRef: "main",
		Branch: "agent/" + t.Name, TaskText: text, Principal: runs.PrincipalFactory, DataClass: t.Spec.DataClass,
		Model: t.Spec.Budget.Model, RoomRef: t.Status.RoomRef, SourceURL: sourceURL(t),
		MaxTokens: t.Spec.Budget.RunTokens, MaxMinutes: t.Spec.Budget.RunMinutes, Traceparent: traceparent(t), Tier: t.Spec.Budget.Tier}
}

// traceparent is the task span's W3C header for its runs (R46); empty when tracing is off.
func traceparent(t *v1alpha1.Task) string {
	if t.Status.Trace == nil {
		return ""
	}
	return tracing.Traceparent(t.Status.Trace.TraceID, t.Status.Trace.SpanID)
}

func (r *Reconciler) startRun(ctx context.Context, t *v1alpha1.Task, s runs.Spec, trigger string) error {
	// The room's lastSeq before the run: its handoff, verdict and end are read after it, never
	// from the start of a long room (EventsSince stops at 10,000 events).
	_, last, err := r.Rooms.Events(ctx, t.Status.RoomRef, 0, 1)
	if err != nil {
		return err
	}
	s.RunID = r.NewRunID()
	if err := r.Runs.Create(ctx, s); err != nil {
		return err
	}
	now := metav1.NewTime(r.Now())
	t.Status.Runs = append(t.Status.Runs, v1alpha1.RunRecord{ID: s.RunID, Role: s.Role, Trigger: trigger,
		Round: t.Status.ReviewRounds, StartSeq: last, Started: &now})
	t.Status.NextTrigger = ""
	r.to(t, phaseFor(s.Role), "")
	narrateLater(t, narrate.Started(t, s, r.Cfg.RoomsURL))
	return nil
}

// current is the task's last run record.
func current(t *v1alpha1.Task) *v1alpha1.RunRecord { return &t.Status.Runs[len(t.Status.Runs)-1] }

// observe copies the run's phase and usage into its record and sums the task's usage. A record's
// tokens never go down, since a stale read of a run must not lower them, so neither does the sum.
func (r *Reconciler) observe(ctx context.Context, t *v1alpha1.Task) (runs.Run, bool, error) {
	cur := current(t)
	run, found, err := r.Runs.Get(ctx, cur.ID)
	if err != nil || !found {
		return run, found, err
	}
	cur.Phase = run.Phase
	cur.Tokens = max(cur.Tokens, run.Tokens)
	var sum int64
	for _, x := range t.Status.Runs {
		sum += x.Tokens
	}
	t.Status.Usage.Tokens = sum
	return run, true, nil
}

// roomReason is the broker's end reason for the task's current run, if the room has it.
func (r *Reconciler) roomReason(ctx context.Context, t *v1alpha1.Task) (string, bool) {
	cur := current(t)
	evs, err := r.roomTail(ctx, t.Status.RoomRef, cur.StartSeq, func(e envelope.Event) bool {
		return e.Type == envelope.StateChanged && e.RunID == cur.ID
	})
	if err != nil {
		r.log().Warn("room log unreadable", "task", t.Name, "err", err)
		return "", false
	}
	_, reason, ok := rooms.LastRunEnd(evs, cur.ID)
	return reason, ok
}

// finished returns the run's end reason once the room has recorded it, or after runEndGrace
// without it ("" while waiting).
func (r *Reconciler) finished(ctx context.Context, t *v1alpha1.Task, run runs.Run) string {
	cur := current(t)
	if cur.Finished == nil {
		now := metav1.NewTime(r.Now())
		cur.Finished = &now
	}
	if reason, ok := r.roomReason(ctx, t); ok {
		return reason
	}
	if r.Now().Sub(cur.Finished.Time) < runEndGrace {
		return ""
	}
	if run.Revoked != "" {
		return run.Revoked
	}
	return strings.ToLower(run.Phase)
}

func (r *Reconciler) implementing(ctx context.Context, t *v1alpha1.Task) error {
	run, found, err := r.observe(ctx, t)
	if err != nil {
		return err
	}
	if !found {
		// SP2 records a deleted claim as Revoked, reason deleted (its P15); say so when it has.
		reason := "run_lost"
		if why, ok := r.roomReason(ctx, t); ok && why == "deleted" {
			reason = why
		}
		return r.end(ctx, t, v1alpha1.PhaseEscalated, reason)
	}
	if t.Status.PullRequest == nil {
		if err := r.detectPR(ctx, t, run); err != nil {
			return err
		}
	}
	if !runs.Terminal(run.Phase) {
		return nil
	}
	reason := r.finished(ctx, t, run)
	if reason == "" {
		return nil
	}
	current(t).Reason = reason
	switch {
	case run.Phase == "Succeeded" && t.Status.PullRequest != nil:
		return r.afterWriter(ctx, t)
	case run.Phase == "Succeeded":
		return r.end(ctx, t, v1alpha1.PhaseNoOp, "no_pr")
	default:
		return r.end(ctx, t, v1alpha1.PhaseEscalated, reason)
	}
}

// afterWriter: phase 1 hands every PR to a human. Phase 3 routes to reviewers, phase 7 to CI.
func (r *Reconciler) afterWriter(_ context.Context, t *v1alpha1.Task) error {
	r.to(t, v1alpha1.PhaseAwaitingHuman, "")
	return nil
}

func (r *Reconciler) detectPR(ctx context.Context, t *v1alpha1.Task, run runs.Run) error {
	n, err := r.Forge.PullRequestForBranch(ctx, "agent/"+t.Name)
	if err != nil || n == 0 {
		return err
	}
	pr, err := r.Forge.PullRequest(ctx, n)
	if err != nil {
		return err
	}
	t.Status.PullRequest = &v1alpha1.PullRequestRef{Number: pr.Number, URL: pr.URL, NodeID: pr.NodeID, HeadSHA: pr.HeadSHA}
	if err := r.Runs.Annotate(ctx, run.ID, map[string]string{runs.AnnPullRequest: pr.URL}); err != nil {
		return err
	}
	if err := r.Forge.AddLabels(ctx, pr.Number, "factory/class:"+t.Spec.PredictedClass); err != nil {
		return err
	}
	d, source, tier, tmpl := r.Now().Sub(t.CreationTimestamp.Time), t.Spec.Source.Kind, t.Spec.Budget.Tier, t.Spec.Template
	record(ctx, func(ctx context.Context) { r.Metrics.TimeToPR(ctx, d, source, tier, tmpl) })
	narrateLater(t, narrate.PROpened(t, pr.Number, pr.URL, run.ID))
	return nil
}

func (r *Reconciler) awaitingHuman(ctx context.Context, t *v1alpha1.Task) error {
	pr, err := r.Forge.PullRequest(ctx, t.Status.PullRequest.Number)
	if err != nil {
		return err
	}
	if r.prEnded(ctx, t, pr) {
		return nil
	}
	if rvs := r.changesRequested(t, pr); len(rvs) > 0 {
		return r.revise(ctx, t, pr, rvs)
	}
	return r.remind(ctx, t, pr)
}

// prEnded ends the task when its pull request was merged or closed. A closed one carrying
// factory/stale was the stale close's (remind): its replay after a lost status write still says so.
func (r *Reconciler) prEnded(ctx context.Context, t *v1alpha1.Task, pr forge.PR) bool {
	class := t.Spec.PredictedClass
	switch pr.State {
	case "MERGED":
		t.Status.PullRequest.MergedBy, t.Status.PullRequest.MergeCommitSHA = pr.MergedBy, pr.MergeCommitSHA
		record(ctx, func(ctx context.Context) { r.Metrics.PROutcome(ctx, class, "human_merged") })
		_ = r.end(ctx, t, v1alpha1.PhaseDone, "merged")
		return true
	case "CLOSED":
		reason := "pr_closed"
		if slices.Contains(pr.Labels, labelStale) {
			reason = "stale"
		}
		record(ctx, func(ctx context.Context) { r.Metrics.PROutcome(ctx, class, "closed") })
		_ = r.end(ctx, t, v1alpha1.PhaseClosed, reason)
		return true
	}
	return false
}
