// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	roomv1 "github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/factory/forge"
	"github.com/Smana/agent-platform/internal/factory/narrate"
	"github.com/Smana/agent-platform/internal/factory/rooms"
	"github.com/Smana/agent-platform/internal/factory/runs"
	"github.com/Smana/agent-platform/internal/factory/taskid"
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
	// A live class may auto-merge its PR, so human review WIP is not its bottleneck; the shadow
	// and unclassified work is exactly what lands on the review queue.
	if len(t.Status.Runs) == 0 && !r.Cfg.Classes[t.Spec.PredictedClass].Live {
		wip, err := r.countTasks(ctx, func(o *v1alpha1.Task) bool { return o.Status.Phase == v1alpha1.PhaseAwaitingHuman })
		if err != nil {
			return false, "", err
		}
		if wip >= r.Cfg.Caps.AwaitingHumanWIP {
			return false, "waiting_review_wip", nil // back-pressure on the reviewers (§6.2)
		}
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
		// One live run per room (C3: a room's runs share its branch). The task's own was recorded
		// before this; anyone else's, a run a human started in the room, is waited for.
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

// runID is the deterministic id of a task's nth run (R48): the claim a replay recomputes, so a
// lost status write finds the run it created instead of minting another. A C2 id, like a Task
// name, so the claim's name is always xplane-run-<id>.
func runID(task string, n int) string {
	return taskid.Name(task + ":run:" + strconv.Itoa(n))
}

// recordExisting records a claim the task's status does not know (R48): the run was created and
// the write that recorded it was lost, so the replay found the claim by its deterministic id.
// The claim is the record — its own role, start seq, head and tokens, whatever its phase — never
// the replay's spec, which may hold a newer brief or a later room seq.
func (r *Reconciler) recordExisting(t *v1alpha1.Task, x runs.Run) error {
	if x.TaskID != t.Name {
		return fmt.Errorf("record run %s of task %s: the id belongs to task %q", x.ID, t.Name, x.TaskID)
	}
	// A verifier's trigger is always "review", and the lost write is what cleared NextRole; an
	// implementer's survives in NextTrigger, which the same write would have cleared.
	trigger := nextTrigger(t)
	if x.Role == "reviewer" || x.Role == "tester" {
		trigger = "review"
	}
	started := metav1.NewTime(r.Now())
	if !x.Created.IsZero() {
		started = metav1.NewTime(x.Created) // when the claim really was created, not the replay
	}
	t.Status.Runs = append(t.Status.Runs, v1alpha1.RunRecord{ID: x.ID, Role: x.Role, Trigger: trigger,
		Round: t.Status.ReviewRounds, Phase: x.Phase, Tokens: x.Tokens, StartSeq: x.StartSeq, HeadSHA: x.Head,
		Started: &started})
	t.Status.NextTrigger, t.Status.NextRole = "", ""
	r.to(t, phaseFor(x.Role), "adopted")
	return nil
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
		Model: t.Spec.Budget.Model, RoomRef: t.Status.RoomRef, SourceURL: sourceURL(t), Queue: runs.QueueFactory,
		MaxTokens: t.Spec.Budget.RunTokens, MaxMinutes: t.Spec.Budget.RunMinutes, Traceparent: traceparent(t), Tier: t.Spec.Budget.Tier}
}

// factorySpentToday is §6.2's "SP3 at admission" for the factory's own principal (R34): today's
// system:factory runs, as the meter annotated them. Shadow only until Task 5.3 reads the day's
// ledger: a stopped run's deleted claim drops out of this sum, its tokens with it.
func (r *Reconciler) factorySpentToday(ctx context.Context) (int64, error) {
	all, err := r.Runs.List(ctx)
	if err != nil {
		return 0, err
	}
	var n int64
	for _, x := range all {
		if x.Principal == runs.PrincipalFactory && sameUTCDay(x.Created, r.Now()) {
			n += x.Tokens
		}
	}
	return n, nil
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
	// from the start of a long room (EventsSince stops at 10,000 events). It rides the claim
	// (AnnStartSeq), so a replay that finds the claim records the seq the run was given.
	last, err := r.Rooms.LastSeq(ctx, t.Status.RoomRef)
	if err != nil {
		return err
	}
	s.RunID, s.StartSeq = runID(t.Name, len(t.Status.Runs)), last
	if err := r.Runs.Create(ctx, s); err != nil {
		// The deterministic id already has a claim: the run was created and its record lost
		// (R48). Recorded, never re-created.
		if !apierrors.IsAlreadyExists(err) {
			return err
		}
		x, found, gerr := r.Runs.Get(ctx, s.RunID)
		if gerr != nil {
			return errors.Join(err, gerr)
		}
		if !found {
			return err
		}
		return r.recordExisting(t, x)
	}
	now := metav1.NewTime(r.Now())
	t.Status.Runs = append(t.Status.Runs, v1alpha1.RunRecord{ID: s.RunID, Role: s.Role, Trigger: trigger,
		Round: t.Status.ReviewRounds, StartSeq: last, HeadSHA: s.Head, Started: &now})
	t.Status.NextTrigger = ""
	r.to(t, phaseFor(s.Role), "")
	narrateLater(t, narrate.Started(t, s, r.Cfg.RoomsURL))
	return nil
}

// current is the task's last run record.
func current(t *v1alpha1.Task) *v1alpha1.RunRecord { return &t.Status.Runs[len(t.Status.Runs)-1] }

// observe lists the task's claims once and copies each one's usage into its record, so a
// reading the meter annotated after a run ended — the current run or any before it — still
// lands (R49). A record's tokens never go down, since a stale read of a run must not lower
// them, so neither does the sum; a deleted claim leaves its record's last reading in place.
// Nor does its phase go back to Pending: the composition reports a started run whose Sandbox
// is not Ready as Pending (its harness exited and the sidecars drain, or a probe fails), and
// the record is what remembers it was admitted.
func (r *Reconciler) observe(ctx context.Context, t *v1alpha1.Task) (runs.Run, bool, error) {
	if len(t.Status.Runs) == 0 {
		return runs.Run{}, false, nil
	}
	all, err := r.Runs.List(ctx)
	if err != nil {
		return runs.Run{}, false, err
	}
	claims := make(map[string]runs.Run, len(all))
	for _, x := range all {
		claims[x.ID] = x
	}
	cur := current(t)
	var sum int64
	for i := range t.Status.Runs {
		rec := &t.Status.Runs[i]
		if x, ok := claims[rec.ID]; ok {
			rec.Tokens = max(rec.Tokens, x.Tokens)
			if rec.ID == cur.ID && (!pending(x.Phase) || pending(rec.Phase)) {
				rec.Phase = x.Phase
			}
		}
		sum += rec.Tokens
	}
	t.Status.Usage.Tokens = sum
	x, found := claims[cur.ID]
	return x, found, nil
}

// pending is a claim that exists but never started: not admitted, no phase written yet.
func pending(phase string) bool { return phase == "" || phase == "Pending" }

// boundPending ends a run that never started (P): nothing else bounds Pending —
// activeDeadlineSeconds counts from the pod's start, and Kueue queues unadmitted work forever.
// A run still Pending past caps.maxPendingMinutes whose record never saw it start (observe) and
// whose claim carries no usage never ran, so deleting it loses no usage; the task escalates as
// run_unschedulable, and a maintainer's /factory retry starts a fresh run.
func (r *Reconciler) boundPending(ctx context.Context, t *v1alpha1.Task, run runs.Run) error {
	if !pending(current(t).Phase) || run.Tokens > 0 || run.Created.IsZero() ||
		r.Now().Sub(run.Created) < time.Duration(r.Cfg.Caps.MaxPendingMinutes)*time.Minute {
		return nil
	}
	if err := r.Runs.Delete(ctx, run.ID); err != nil {
		return err
	}
	return r.end(ctx, t, v1alpha1.PhaseEscalated, "run_unschedulable")
}

// roomReason is the broker's end reason for the task's current run, if the room has it.
func (r *Reconciler) roomReason(ctx context.Context, t *v1alpha1.Task) (string, bool) {
	cur := current(t)
	evs, _, err := r.roomTail(ctx, t.Status.RoomRef, cur.StartSeq, func(e envelope.Event) bool {
		return e.Type == envelope.StateChanged && e.RunID == cur.ID
	})
	if err != nil {
		r.log().Warn("room log unreadable", "task.id", t.Name, "run.id", cur.ID, "err", err)
		return "", false
	}
	end, ok := rooms.LastRunEnd(evs, cur.ID)
	return end.Reason, ok
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
		return r.end(ctx, t, v1alpha1.PhaseEscalated, r.lostReason(ctx, t))
	}
	if err := r.boundPending(ctx, t, run); err != nil {
		return err
	}
	if t.Status.PullRequest == nil {
		if err := r.detectPR(ctx, t, run); err != nil {
			return err
		}
	}
	if !runs.Terminal(run.Phase) {
		if s, err := r.stuck(ctx, t, run); err != nil || s {
			if err != nil {
				return err
			}
			return r.end(ctx, t, v1alpha1.PhaseEscalated, "stuck")
		}
		return nil
	}
	reason := r.finished(ctx, t, run)
	if reason == "" {
		return nil
	}
	current(t).Reason = reason
	r.interventions(ctx, t)
	// R38: the triager's output reaches a public implementer only through a human. Its summary
	// stays in the room (internal); the issue gets the room link and the next step, nothing else.
	if current(t).Role == "triager" {
		if run.Phase != "Succeeded" {
			return r.end(ctx, t, v1alpha1.PhaseEscalated, reason)
		}
		evs, _, err := r.Rooms.EventsSince(ctx, t.Status.RoomRef, current(t).StartSeq)
		if err != nil {
			return err
		}
		if !handedOffTo(evs, "implementer") {
			return r.end(ctx, t, v1alpha1.PhaseNoOp, "no_action")
		}
		narrateLater(t, narrate.ProposalReady(t, r.Cfg.RoomsURL))
		return r.end(ctx, t, v1alpha1.PhaseDone, "proposal_ready")
	}
	switch {
	case run.Phase == "Succeeded" && t.Status.PullRequest != nil:
		return r.afterWriter(ctx, t)
	case run.Phase == "Succeeded":
		return r.end(ctx, t, v1alpha1.PhaseNoOp, "no_pr")
	default:
		return r.end(ctx, t, v1alpha1.PhaseEscalated, reason)
	}
}

// handedOffTo reports whether any event is a handoff to role: the triager's proposal exists only
// if it made one (R38).
func handedOffTo(evs []envelope.Event, role string) bool {
	for _, e := range evs {
		var p envelope.HandoffPayload
		if e.Type == envelope.Handoff && json.Unmarshal(e.Payload, &p) == nil && p.ToRole == role {
			return true
		}
	}
	return false
}

// lostReason is why a run of the task vanished: SP2 records a deleted claim as Revoked, reason
// deleted (its P15); say so when it has.
func (r *Reconciler) lostReason(ctx context.Context, t *v1alpha1.Task) string {
	if why, ok := r.roomReason(ctx, t); ok && why == "deleted" {
		return why
	}
	return "run_lost"
}

// afterWriter: a revision a maintainer asked for goes straight back to the maintainer; otherwise
// the template's first verifier after the implementer runs, or the task is ready (solo).
func (r *Reconciler) afterWriter(ctx context.Context, t *v1alpha1.Task) error {
	if current(t).Trigger == "human" {
		r.to(t, v1alpha1.PhaseAwaitingHuman, "")
		return nil
	}
	if current(t).Trigger == "ci" {
		return r.ready(ctx, t) // a CI fix goes back to CI, not to another review round
	}
	if next := r.nextVerifier(t, "implementer"); next != "" {
		r.requestVerifier(t, next)
		return nil
	}
	return r.ready(ctx, t)
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
	r.countApproves(ctx, t, pr)
	if rvs := r.changesRequested(t, pr); len(rvs) > 0 {
		return r.revise(ctx, t, pr, rvs)
	}
	return r.remind(ctx, t, pr)
}

// countApproves counts each maintainer's APPROVED review once (§7: how dark the factory is):
// a gate that still needed a human's approve is not dark. The review id rides status.handled,
// the same list that dedups request-changes reviews and commands.
func (r *Reconciler) countApproves(ctx context.Context, t *v1alpha1.Task, pr forge.PR) {
	for _, rv := range pr.Reviews {
		if rv.State == "APPROVED" && r.Cfg.IsMaintainer(rv.Author) && !slices.Contains(t.Status.Handled, rv.ID) {
			markHandled(t, rv.ID)
			record(ctx, func(ctx context.Context) { r.Metrics.Intervention(ctx, "approve") })
		}
	}
}

// prEnded ends the task when its pull request was merged or closed. A closed one carrying
// factory/stale was the stale close's (remind): its replay after a lost status write still says so.
func (r *Reconciler) prEnded(ctx context.Context, t *v1alpha1.Task, pr forge.PR) bool {
	class := t.Spec.PredictedClass
	switch pr.State {
	case "MERGED":
		t.Status.PullRequest.MergedBy, t.Status.PullRequest.MergeCommitSHA = pr.MergedBy, pr.MergeCommitSHA
		// R41: like the CI gate's bypass record, the timestamp is what puts the merge in the
		// class's breaker window — without it a demoted class' human merges never refill it.
		now := metav1.NewTime(r.Now())
		t.Status.PullRequest.MergedAt = &now
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
