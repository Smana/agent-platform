// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"errors"
	"log/slog"
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
// from afterSeq with its resume cursor, the room's queue, and task_state messages.
type RoomLog interface {
	EventsSince(ctx context.Context, room string, afterSeq int64) ([]envelope.Event, int64, error)
	Events(ctx context.Context, room string, afterSeq int64, limit int) ([]envelope.Event, int64, error)
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
}

// metrics is the part of fmetrics.Set the reconciler records.
type metrics interface {
	TimeToPR(ctx context.Context, d time.Duration, source, tier, template string)
	PROutcome(ctx context.Context, class, outcome string)
	TaskTokens(ctx context.Context, tokens int64, tier, template, predictedClass string)
	Intervention(ctx context.Context, kind string)
	TraceExportAbandoned(ctx context.Context)
}

// A task's span unexported at its end is retried every spanRetry, and given up spanGiveUp after
// the end (ruling ST2): each try costs the single worker up to tracing.ExportTimeout, so a
// collector outage must not grow an unbounded backlog of them.
const (
	spanRetry  = 15 * time.Minute
	spanGiveUp = 24 * time.Hour
)

// Reconciler is the Task state machine (§4). One reconcile per task every poll interval and on
// every change of one of its AgentRuns; one worker, so the caps are counted without a race
// between two tasks starting at once.
type Reconciler struct {
	Client    client.Client
	Namespace string
	Cfg       *config.Config
	Forge     taskForge
	Runs      RunClient
	Rooms     RoomLog
	Triage    triage.Triager
	Metrics   metrics
	Now       func() time.Time
	NewRunID  func() string
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
	if ended && len(t.Status.Outbox) == 0 && !r.spanDue(&t) {
		return ctrl.Result{}, nil
	}
	before := t.Status.DeepCopy()
	if t.Status.Phase == "" {
		r.to(&t, v1alpha1.PhaseReceived, "")
	}
	fx := &effects{}
	var err error
	if !ended {
		err = r.step(context.WithValue(ctx, effectsKey{}, fx), &t)
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
	if err != nil || v1alpha1.TerminalPhase(t.Status.Phase) {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: r.Cfg.Poll.Tasks.Duration}, nil
}

func (r *Reconciler) step(ctx context.Context, t *v1alpha1.Task) error {
	stop, why, err := r.stopRequested(ctx, t)
	if err != nil {
		return err
	}
	if stop {
		return r.stop(ctx, t, why)
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
	case v1alpha1.PhaseAwaitingHuman:
		return r.awaitingHuman(ctx, t)
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
// so an outage never loses it: drain posts it, now or on a later reconcile. A key queued twice is
// posted once: Post skips a key already in status.narrated.
func narrateLater(t *v1alpha1.Task, e narrate.Event) {
	n := target(t)
	if n == 0 {
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

// end moves a task to a terminal or escalated phase and says why, once.
func (r *Reconciler) end(ctx context.Context, t *v1alpha1.Task, phase, reason string) error {
	r.to(t, phase, reason)
	if v1alpha1.TerminalPhase(phase) {
		tokens, tier, tmpl, class := t.Status.Usage.Tokens, t.Spec.Budget.Tier, t.Spec.Template, t.Spec.PredictedClass
		record(ctx, func(ctx context.Context) { r.Metrics.TaskTokens(ctx, tokens, tier, tmpl, class) })
	}
	narrateLater(t, narrate.Ended(t, phase, reason))
	return nil
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
			r.log().Warn("task span given up", "task", t.Name, "err", err)
			return
		}
		r.log().Warn("task span not exported", "task", t.Name, "err", err)
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

func (r *Reconciler) queued(ctx context.Context, t *v1alpha1.Task) error {
	if adopted, err := r.adopt(ctx, t); err != nil || adopted {
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
	if t.Status.PullRequest != nil && len(t.Status.Runs) > 0 {
		if done, err := r.lateReviews(ctx, t); err != nil || done {
			return err
		}
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
		r.log().Warn("queue consume failed", "task", t.Name, "err", err)
	}
	return nil
}
