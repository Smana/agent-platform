// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/forge"
	"github.com/Smana/agent-platform/internal/factory/killswitch"
	"github.com/Smana/agent-platform/internal/factory/narrate"
	"github.com/Smana/agent-platform/internal/factory/rooms"
	"github.com/Smana/agent-platform/internal/factory/runs"
	"github.com/Smana/agent-platform/internal/factory/tracing"
	"github.com/Smana/agent-platform/internal/factory/triage"
)

// RunClient is the AgentRun API as the reconciler uses it (runs.Client).
type RunClient interface {
	Create(ctx context.Context, s runs.Spec) error
	Get(ctx context.Context, id string) (runs.Run, bool, error)
	List(ctx context.Context) ([]runs.Run, error)
	Annotate(ctx context.Context, id string, kv map[string]string) error
	Delete(ctx context.Context, id string) error
}

// RoomLog is the broker's system API as the reconciler uses it (rooms.Client): the room's log
// from afterSeq with its resume cursor, its current seq, the room's queue, and task_state messages.
type RoomLog interface {
	EventsSince(ctx context.Context, room string, afterSeq int64) ([]envelope.Event, int64, error)
	LastSeq(ctx context.Context, room string) (int64, error)
	Enqueue(ctx context.Context, room, stream, text string, clientSeq int64) error
	Queue(ctx context.Context, room string) ([]rooms.Queued, error)
	Consume(ctx context.Context, room string, refs []int64, runID string) error
	TaskState(ctx context.Context, room, text string, clientSeq int64) error
}

// taskForge is the part of the forge the reconciler uses, as the factory App.
type taskForge interface {
	RecentComments(ctx context.Context, number int) ([]forge.Comment, error)
	Comment(ctx context.Context, number int, body string) error
	AddLabels(ctx context.Context, number int, labels ...string) error
	PullRequestForBranch(ctx context.Context, branch string) (int, error)
	PullRequest(ctx context.Context, number int) (forge.PR, error)
	ClosePR(ctx context.Context, number int) error
	RemoveLabel(ctx context.Context, number int, label string) error
}

// metrics is the part of fmetrics.Set the reconciler records.
type metrics interface {
	TimeToPR(ctx context.Context, d time.Duration, source, tier, template string)
	PROutcome(ctx context.Context, class, outcome string)
	TaskTokens(ctx context.Context, tokens int64, tier, template, predictedClass string)
	Intervention(ctx context.Context, kind string)
	TierFit(ctx context.Context, classifier, tier, fit string, control bool)
	Revoked(ctx context.Context, reason string)
	TraceExportAbandoned(ctx context.Context)
	ClassMismatch(ctx context.Context, predicted, matched string)
}

// A task's span unexported at its end is retried every spanRetry, and given up spanGiveUp after
// the end (ruling ST2): each try costs the single worker up to tracing.ExportTimeout, so a
// collector outage must not grow an unbounded backlog of them.
const (
	spanRetry  = 15 * time.Minute
	spanGiveUp = 24 * time.Hour
)

// A pull request no maintainer has touched for RemindAfter gets a reminder, and is closed with
// factory/stale after StaleAfter (§6.3), never sooner than reminderNotice after that reminder.
const (
	RemindAfter    = 48 * time.Hour
	StaleAfter     = 14 * 24 * time.Hour
	reminderNotice = 24 * time.Hour
)

// escalatedPoll is how often an escalated task is reconciled. It waits only for a maintainer's
// /factory retry or the end of its pull request, which nothing bounds in number (§6.3 gives
// Escalated no timeout), and each poll costs GitHub calls the whole factory shares.
const escalatedPoll = 5 * time.Minute

// Reconciler is the Task state machine (§4). One reconcile per task every poll interval and on
// every change of one of its AgentRuns; one worker, so the caps are counted without a race
// between two tasks starting at once. Run ids are derived from the task (R48), never random.
type Reconciler struct {
	Client    client.Client
	Namespace string
	Cfg       *config.Config
	Forge     taskForge
	Merger    forge.Merger // the merger App: checks, merges and reverts (R16)
	Runs      RunClient
	Rooms     RoomLog
	Triage    triage.Triager
	Metrics   metrics
	Now       func() time.Time
	Nonce     func() string
	Log       *slog.Logger
	Trace     tracing.Sink // nil: tracing off (R46)
}

// SetupWithManager watches Tasks and the AgentRuns that carry a task label.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	run := &unstructured.Unstructured{}
	run.SetGroupVersionKind(runs.GVK())
	return ctrl.NewControllerManagedBy(mgr).For(&v1alpha1.Task{}).
		Watches(run, handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
			id := o.GetLabels()[runs.LabelTask]
			if id == "" {
				return nil
			}
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: r.Namespace, Name: id}}}
		})).
		WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
		Complete(r)
}

// Reconcile moves one task one step and writes its status when the step changed it.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var t v1alpha1.Task
	if err := r.Client.Get(ctx, req.NamespacedName, &t); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	ended := v1alpha1.TerminalPhase(t.Status.Phase)
	// A requested revert and an open revert PR are the two exceptions to leaving a terminal
	// task alone (§6.4): the factory owes the revert its watch, and a maintainer's factory/revert
	// its action, even after the task itself has ended.
	revertWait := r.revertable(&t) || r.revertPending(&t)
	if ended && !revertWait && len(t.Status.Outbox) == 0 && !r.spanDue(&t) && !r.settling(&t) {
		return ctrl.Result{}, nil
	}
	before := t.Status.DeepCopy()
	if t.Status.Phase == "" {
		r.to(&t, v1alpha1.PhaseReceived, "")
	}
	fx := &effects{}
	var err error
	switch {
	case !ended, revertWait:
		err = r.step(context.WithValue(ctx, effectsKey{}, fx), &t)
	case r.settling(&t):
		err = r.settle(context.WithValue(ctx, effectsKey{}, fx), &t)
	}
	if !equality.Semantic.DeepEqual(*before, t.Status) {
		if uerr := r.Client.Status().Update(ctx, &t); uerr != nil {
			return ctrl.Result{}, errors.Join(err, uerr)
		}
		for _, f := range fx.after {
			f(ctx)
		}
	}
	// Only a written outbox is posted (review M-a), and only a written end exports the task's span:
	// a write that conflicts posts and exports nothing, and its replay may take another path. The
	// second write records what was posted and exported.
	if len(t.Status.Outbox) > 0 || r.spanDue(&t) {
		queued := t.Status.DeepCopy()
		err = errors.Join(err, r.drain(ctx, &t))
		r.endTrace(ctx, &t)
		if !equality.Semantic.DeepEqual(*queued, t.Status) {
			if uerr := r.Client.Status().Update(ctx, &t); uerr != nil {
				return ctrl.Result{}, errors.Join(err, uerr)
			}
			if t.Status.Trace != nil && t.Status.Trace.ExportAbandoned && !queued.Trace.ExportAbandoned {
				r.Metrics.TraceExportAbandoned(ctx) // once: counted with the write that records it
			}
		}
	}
	if err == nil && r.spanDue(&t) {
		return ctrl.Result{RequeueAfter: spanRetry}, nil
	}
	if v1alpha1.TerminalPhase(t.Status.Phase) {
		// The revert watch polls: a revert PR merges, closes or stalls on GitHub's clock, not ours.
		if err == nil && (r.revertable(&t) || r.revertPending(&t)) {
			return ctrl.Result{RequeueAfter: r.Cfg.Poll.Tasks.Duration}, nil
		}
		if err == nil && r.settling(&t) {
			return ctrl.Result{RequeueAfter: r.settleLeft(&t)}, nil // for the settle, at its window
		}
		return ctrl.Result{}, err
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if t.Status.Phase == v1alpha1.PhaseEscalated {
		return ctrl.Result{RequeueAfter: escalatedPoll}, nil
	}
	return ctrl.Result{RequeueAfter: r.Cfg.Poll.Tasks.Duration}, nil
}

func (r *Reconciler) step(ctx context.Context, t *v1alpha1.Task) error {
	// A terminal task reaches step only for its revert (§6.4); the stop machinery must not
	// rewrite an ended task into Stopped, and its runs are already over.
	if !v1alpha1.TerminalPhase(t.Status.Phase) {
		stop, why, err := r.stopRequested(ctx, t)
		if err != nil {
			return err
		}
		if stop {
			return r.stop(ctx, t, why)
		}
	}
	switch t.Status.Phase {
	case v1alpha1.PhaseReceived:
		return r.received(ctx, t)
	case v1alpha1.PhaseTriaged:
		return r.triaged(ctx, t)
	case v1alpha1.PhaseQueued:
		return r.queued(ctx, t)
	case v1alpha1.PhaseImplementing:
		return r.implementing(ctx, t)
	case v1alpha1.PhaseReviewing:
		return r.reviewing(ctx, t)
	case v1alpha1.PhaseAwaitingHuman:
		return r.awaitingHuman(ctx, t)
	case v1alpha1.PhaseEscalated:
		return r.escalated(ctx, t)
	case v1alpha1.PhaseAwaitingCI:
		return r.awaitingCI(ctx, t)
	case v1alpha1.PhaseAutoMerging:
		return r.autoMerging(ctx, t)
	case v1alpha1.PhaseVerifying:
		return r.verifying(ctx, t)
	case v1alpha1.PhaseReverted:
		return r.revertWatch(ctx, t) // reached only while revertPending
	case v1alpha1.PhaseDone:
		return r.revert(ctx, t, "revert_requested") // reached only when revertable
	}
	return nil
}

// effects are what a step records outside the Task: metrics, run once the status that caused them
// is written. A conflicting write replays the step, which would otherwise count it twice.
type effects struct{ after []func(context.Context) }

type effectsKey struct{}

// record defers f until the step's status is written; outside Reconcile it runs at once.
func record(ctx context.Context, f func(context.Context)) {
	if fx, ok := ctx.Value(effectsKey{}).(*effects); ok {
		fx.after = append(fx.after, f)
		return
	}
	f(ctx)
}

func (r *Reconciler) log() *slog.Logger {
	if r.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return r.Log
}

func (r *Reconciler) to(t *v1alpha1.Task, phase, reason string) {
	if t.Status.Phase != phase {
		now := metav1.NewTime(r.Now())
		t.Status.PhaseSince = &now
	}
	t.Status.Phase, t.Status.Reason = phase, reason
}

func (r *Reconciler) narrator() narrate.Narrator {
	return narrate.Narrator{Forge: r.Forge, Login: r.Cfg.FactoryLogin}
}

func stopReasons() map[string]string {
	return map[string]string{"true": "stopped_by_annotation", "label": "stopped_by_label", "superseded": "superseded"}
}

func (r *Reconciler) stopRequested(ctx context.Context, t *v1alpha1.Task) (bool, string, error) {
	on, err := killswitch.Engaged(ctx, r.Client, r.Namespace)
	if err != nil {
		return false, "", err // never stop on a transient error
	}
	if on {
		return true, "kill_switch", nil
	}
	if v := t.Annotations[v1alpha1.AnnotationStop]; v != "" {
		if why, ok := stopReasons()[v]; ok {
			return true, why, nil
		}
		return true, "stopped_by_annotation", nil
	}
	return false, "", nil
}

// stop: every live run of the task is annotated revoked=manual, then every run deleted (§6.1).
// The runs are the cluster's, not status's (ruling SN): a run whose record was lost to a
// conflicting status write is still the task's, and still stopped.
func (r *Reconciler) stop(ctx context.Context, t *v1alpha1.Task, why string) error {
	all, err := r.Runs.List(ctx)
	if err != nil {
		return err
	}
	for _, run := range all {
		if run.TaskID != t.Name {
			continue
		}
		if !runs.Terminal(run.Phase) {
			if err := r.Runs.Annotate(ctx, run.ID, map[string]string{runs.AnnRevoked: "manual"}); err != nil {
				return err
			}
			// Counted now, not with the status write: a replay finds the claim gone or already
			// revoked, so a deferred count would be lost. A run that carried a revoke was counted
			// by whoever wrote it, the kill switch's sweeper included (F30).
			if run.Revoked == "" {
				r.Metrics.Revoked(ctx, "manual")
			}
		}
		if err := r.Runs.Delete(ctx, run.ID); err != nil {
			return err
		}
	}
	if why != "kill_switch" {
		record(ctx, func(ctx context.Context) { r.Metrics.Intervention(ctx, "stop") })
	}
	return r.end(ctx, t, v1alpha1.PhaseStopped, why)
}

// narrateLater queues e in the task's outbox (ruling SO), written with the transition that caused it,
// so an outage never loses it: drain posts it, now or on a later reconcile. A key already queued
// or posted is not queued again, so a timer that re-fires every poll (the reminder) writes no
// status; Post would skip it anyway.
func narrateLater(t *v1alpha1.Task, e narrate.Event) { narrateOn(t, target(t), e) }

// narrateOn is narrateLater on issue or pull request n.
func narrateOn(t *v1alpha1.Task, n int, e narrate.Event) {
	if n == 0 || slices.Contains(t.Status.Narrated, e.Key) ||
		slices.ContainsFunc(t.Status.Outbox, func(o v1alpha1.Narration) bool { return o.Key == e.Key }) {
		return
	}
	t.Status.Outbox = append(t.Status.Outbox, v1alpha1.Narration{Key: e.Key, Number: n, Body: e.Body})
}

// drain posts the outbox in order and stops at the first failure, which the next reconcile
// retries. A post that landed but failed is found by its marker then, not posted twice.
func (r *Reconciler) drain(ctx context.Context, t *v1alpha1.Task) error {
	for len(t.Status.Outbox) > 0 {
		o := t.Status.Outbox[0]
		if err := r.narrator().Post(ctx, t, o.Number, narrate.Event{Key: o.Key, Body: o.Body}); err != nil {
			return err
		}
		t.Status.Outbox = t.Status.Outbox[1:]
	}
	t.Status.Outbox = nil
	return nil
}

// target is where a task narrates: its issue, else its PR (R28), else nowhere.
func target(t *v1alpha1.Task) int {
	if t.Spec.Issue > 0 {
		return t.Spec.Issue
	}
	if t.Status.PullRequest != nil {
		return t.Status.PullRequest.Number
	}
	return 0
}

// end moves a task to a terminal or escalated phase and says why, once. The task's tokens are
// not recorded here: the settle does that, once, after the meter's late readings have had their
// window (R49). An escalation names the maintainers (§6.3); the outcome records tier fit and the
// task.final line the moment the task first ends.
func (r *Reconciler) end(ctx context.Context, t *v1alpha1.Task, phase, reason string) error {
	r.to(t, phase, reason)
	narrateLater(t, narrate.Ended(t, phase, reason, r.Cfg.Maintainers...))
	r.outcome(ctx, t)
	return nil
}

// settleWindow bounds how long an ended task keeps refreshing its usage before the total is
// recorded (R49): twice the meter's period — one reading can trail one tick — plus the travel of
// the annotation the reading becomes.
func (r *Reconciler) settleWindow() time.Duration {
	return 2*r.Cfg.Poll.Meter.Duration + 30*time.Second
}

// settling: the task has ended with runs whose usage has not settled yet (R49). A task without
// runs has nothing to settle: no claim exists for the meter to annotate.
func (r *Reconciler) settling(t *v1alpha1.Task) bool {
	return v1alpha1.TerminalPhase(t.Status.Phase) && len(t.Status.Runs) > 0 && !t.Status.UsageSettled
}

// settle refreshes a settling task's records from their claims, and once the window closes
// records the task's tokens and marks the usage settled. The metric rides the same status write
// as the mark, so a conflicting write replays both; the write that lands records them once.
func (r *Reconciler) settle(ctx context.Context, t *v1alpha1.Task) error {
	if _, _, err := r.observe(ctx, t); err != nil {
		return err
	}
	if t.Status.PhaseSince != nil && r.Now().Sub(t.Status.PhaseSince.Time) < r.settleWindow() {
		return nil
	}
	t.Status.UsageSettled = true
	tokens, tier, tmpl, class := t.Status.Usage.Tokens, t.Spec.Budget.Tier, t.Spec.Template, t.Spec.PredictedClass
	record(ctx, func(ctx context.Context) { r.Metrics.TaskTokens(ctx, tokens, tier, tmpl, class) })
	return nil
}

// settleLeft is how much of the settle window an ended task still owes: it is requeued for its
// settle, never on the poll interval an ended task otherwise skips.
func (r *Reconciler) settleLeft(t *v1alpha1.Task) time.Duration {
	if t.Status.PhaseSince == nil {
		return r.Cfg.Poll.Tasks.Duration
	}
	return max(r.settleWindow()-r.Now().Sub(t.Status.PhaseSince.Time), time.Second)
}

// spanDue: the task has ended and its root span is still to export (R46).
func (r *Reconciler) spanDue(t *v1alpha1.Task) bool {
	tr := t.Status.Trace
	return r.Trace != nil && v1alpha1.TerminalPhase(t.Status.Phase) && tr != nil && !tr.Exported && !tr.ExportAbandoned
}

// endTrace exports the task's root span (R46), from the label's acceptance (the Task's creation)
// to the end its status recorded, so a retry exports the same span: at least once, since a
// conflict on the write that records Exported replays it. Best effort: a lost span never holds a
// task. A failed export is retried every spanRetry and given up spanGiveUp after the end (ST2).
func (r *Reconciler) endTrace(ctx context.Context, t *v1alpha1.Task) {
	if !r.spanDue(t) {
		return
	}
	end := r.Now()
	if t.Status.PhaseSince != nil {
		end = t.Status.PhaseSince.Time
	}
	tr := t.Status.Trace
	err := r.Trace.Export(ctx, tracing.Task{TraceID: tr.TraceID, SpanID: tr.SpanID, TaskID: t.Name, Tier: t.Spec.Budget.Tier,
		Phase: t.Status.Phase, Reason: t.Status.Reason, Start: t.CreationTimestamp.Time, End: end})
	if err != nil {
		if r.Now().Sub(end) >= spanGiveUp {
			tr.ExportAbandoned = true
			r.log().Warn("task span given up", "task.id", t.Name, "err", err)
			return
		}
		r.log().Warn("task span not exported", "task.id", t.Name, "err", err)
		return
	}
	tr.Exported = true
}

func (r *Reconciler) countTasks(ctx context.Context, keep func(*v1alpha1.Task) bool) (int, error) {
	var l v1alpha1.TaskList
	if err := r.Client.List(ctx, &l, client.InNamespace(r.Namespace)); err != nil {
		return 0, err
	}
	n := 0
	for i := range l.Items {
		if keep(&l.Items[i]) {
			n++
		}
	}
	return n, nil
}

func sameUTCDay(a, b time.Time) bool {
	return a.UTC().Format(time.DateOnly) == b.UTC().Format(time.DateOnly)
}

// admit is R6's text cap and §6.2's daily task cap; a rejected task does not count.
func (r *Reconciler) admit(ctx context.Context, t *v1alpha1.Task) (string, error) {
	if len(t.Spec.Text) > r.Cfg.Caps.MaxTextBytes {
		return "text_too_long", nil
	}
	today, err := r.countTasks(ctx, func(o *v1alpha1.Task) bool {
		return o.Name != t.Name && o.Status.Phase != "" && o.Status.Phase != v1alpha1.PhaseRejected &&
			sameUTCDay(o.CreationTimestamp.Time, r.Now())
	})
	if err != nil {
		return "", err
	}
	if today >= r.Cfg.Caps.TasksPerDay {
		return "daily_task_cap", nil
	}
	return "", nil
}

func (r *Reconciler) received(ctx context.Context, t *v1alpha1.Task) error {
	// A RunLore task's issue is written by the intake just after its creation (FA-8): wait for
	// it, so the first narration has a place to land. Past two minutes the intake is gone and
	// the task proceeds without one.
	if t.Spec.Source.Kind == "runlore" && t.Spec.Issue == 0 && r.Now().Sub(t.CreationTimestamp.Time) < 2*time.Minute {
		return nil
	}
	reason, err := r.admit(ctx, t)
	if err != nil {
		return err
	}
	if reason != "" {
		return r.end(ctx, t, v1alpha1.PhaseRejected, reason)
	}
	if r.Trace != nil && t.Status.Trace == nil { // R46: the task's root span, minted once at acceptance
		tr, sp := tracing.Mint()
		t.Status.Trace = &v1alpha1.TraceRef{TraceID: tr, SpanID: sp}
	}
	d, err := r.Triage.Triage(ctx, t)
	if err != nil {
		return err
	}
	t.Spec.Template, t.Spec.PredictedClass, t.Spec.DataClass, t.Spec.Budget = d.Template, d.PredictedClass, d.DataClass, d.Budget
	status := t.Status // Update returns the server's status into t; keep ours
	if err := r.Client.Update(ctx, t); err != nil {
		return err
	}
	t.Status = status
	t.Status.Classification = &d.Classification
	t.Status.ConfigHash = r.Cfg.Hash
	r.to(t, v1alpha1.PhaseTriaged, "")
	return nil
}

// triaged gives the task its room (R2). A room of that name the factory did not make escalates:
// its runs would inherit another owner's data class (C3).
func (r *Reconciler) triaged(ctx context.Context, t *v1alpha1.Task) error {
	err := rooms.Ensure(ctx, r.Client, r.Namespace, t.Name, t.Spec.DataClass, t.Spec.Repository)
	if errors.Is(err, rooms.ErrForeignRoom) {
		return r.end(ctx, t, v1alpha1.PhaseEscalated, "foreign_room")
	}
	if err != nil {
		return err
	}
	t.Status.RoomRef = t.Name
	r.to(t, v1alpha1.PhaseQueued, "")
	return nil
}

// runFits: the task's cap still holds one whole run (owner, 2026-10-07). A run may spend up to
// RunTokens before the meter revokes it, so admitting on "under the cap" let a task end 1.5 M past
// it; a resume follows the same rule.
func runFits(t *v1alpha1.Task) bool {
	b := t.Spec.Budget
	return b.TaskTokens <= 0 || b.TaskTokens-t.Status.Usage.Tokens >= b.RunTokens
}

func (r *Reconciler) queued(ctx context.Context, t *v1alpha1.Task) error {
	// A claim of the next run's deterministic id is a run the status does not know: the write
	// that recorded it was lost (R48). Recorded, never duplicated, and before the caps: the run
	// already holds its slot.
	x, found, err := r.Runs.Get(ctx, runID(t.Name, len(t.Status.Runs)))
	if err != nil {
		return err
	}
	if found {
		return r.recordExisting(t, x)
	}
	// The records are refreshed here too (R49): a task waiting in Queued still holds ended runs
	// whose late readings must land.
	if _, _, err := r.observe(ctx, t); err != nil {
		return err
	}
	ok, why, err := r.slotFree(ctx, t)
	if err != nil {
		return err
	}
	if !ok {
		t.Status.Reason = why
		return nil
	}
	// A pull request merged or closed meanwhile ends the wait with no run, so it is read before
	// the caps that guard a run's start: a landed task never escalates for a budget it no longer
	// needs.
	var pr forge.PR
	late := t.Status.PullRequest != nil && len(t.Status.Runs) > 0
	if late {
		var done bool
		if pr, done, err = r.lateReviews(ctx, t); err != nil || done {
			return err
		}
	}
	if !runFits(t) {
		if r.Cfg.Budgets.EnforceTask {
			return r.end(ctx, t, v1alpha1.PhaseEscalated, "budget-task") // no new run past the task cap (C5)
		}
		// R3: shadow first, a week of numbers before the flag flips.
		record(ctx, func(ctx context.Context) { r.Metrics.Revoked(ctx, "budget-task-shadow") })
		r.log().Info("task cap has no room for a whole run (shadow)", "task.id", t.Name, "used", t.Status.Usage.Tokens,
			"run", t.Spec.Budget.RunTokens, "cap", t.Spec.Budget.TaskTokens)
	}
	// R34: the factory's own day, checked before every run it starts, not only by the meter.
	if limit := r.Cfg.Budgets.FactoryDaily; limit > 0 {
		spent, err := r.factorySpentToday(ctx)
		if err != nil {
			return err
		}
		if spent >= limit {
			if r.Cfg.Budgets.EnforcePrincipal {
				t.Status.Reason = "waiting_daily_budget" // no new factory run until 00:00 UTC
				return nil
			}
			record(ctx, func(ctx context.Context) { r.Metrics.Revoked(ctx, "budget-principal-shadow") })
		}
	}
	if len(t.Status.Runs) == 0 {
		// The snapshot, once, in the room of record: later runs read it there, never the live
		// issue (§1, T1). Its clientSeq is the task's room ledger (ruling SK), so a retry reposts
		// the same seq, which the broker keeps once. A room the broker has no log for yet waits.
		err := narrate.Room(ctx, r.Rooms, t, "snapshot", SnapshotMessage(t, r.Nonce()),
			func(ctx context.Context) error { return r.Client.Status().Update(ctx, t) })
		switch {
		case errors.Is(err, rooms.ErrNoRoom):
			t.Status.Reason = "waiting_room_log"
			return nil
		case errors.Is(err, rooms.ErrNotPermitted): // FR-1 not enabled yet: visible, and retried with backoff
			t.Status.Reason = "waiting_broker_permission"
			return err
		case err != nil:
			return err
		}
	}
	if late && t.Status.NextRole != "" {
		return r.startVerifier(ctx, t, pr)
	}
	// R38: a task of a [triager] template always runs the triager, retry included: no implementer
	// run ever starts from an internal-origin task. A missing template is no team, so it is not a
	// triager one either: the config's validation cannot guard a name removed after a task was
	// triaged.
	if roles := r.Cfg.Templates[t.Spec.Template].Roles; len(roles) > 0 && roles[0] == "triager" {
		s := r.implementerSpec(t, TriagerBrief(t, r.Nonce()))
		s.Role = "triager"
		return r.startRun(ctx, t, s, nextTrigger(t))
	}
	s, refs, trigger, err := r.nextImplementer(ctx, t)
	if err != nil {
		return err
	}
	if err := r.startRun(ctx, t, s, trigger); err != nil {
		return err
	}
	// Consumed once the run exists, and only what its brief quoted (F-A). A failure only means
	// the next brief repeats them.
	if err := r.Rooms.Consume(ctx, t.Status.RoomRef, refs, current(t).ID); err != nil {
		r.log().Warn("queue consume failed", "task.id", t.Name, "run.id", current(t).ID, "err", err)
	}
	return nil
}
