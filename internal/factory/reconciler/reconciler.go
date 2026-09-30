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

// RoomLog reads a room's log from afterSeq, returning the resume cursor (rooms.Client).
type RoomLog interface {
	EventsSince(ctx context.Context, room string, afterSeq int64) ([]envelope.Event, int64, error)
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
}

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
	if v1alpha1.TerminalPhase(t.Status.Phase) {
		return ctrl.Result{}, nil
	}
	before := t.Status.DeepCopy()
	if t.Status.Phase == "" {
		r.to(&t, v1alpha1.PhaseReceived, "")
	}
	err := r.step(ctx, &t)
	if !equality.Semantic.DeepEqual(*before, t.Status) {
		if uerr := r.Client.Status().Update(ctx, &t); uerr != nil {
			return ctrl.Result{}, errors.Join(err, uerr)
		}
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
func (r *Reconciler) stop(ctx context.Context, t *v1alpha1.Task, why string) error {
	for _, rec := range t.Status.Runs {
		run, found, err := r.Runs.Get(ctx, rec.ID)
		if err != nil {
			return err
		}
		if !found {
			continue
		}
		if !runs.Terminal(run.Phase) {
			if err := r.Runs.Annotate(ctx, rec.ID, map[string]string{runs.AnnRevoked: "manual"}); err != nil {
				return err
			}
		}
		if err := r.Runs.Delete(ctx, rec.ID); err != nil {
			return err
		}
	}
	if why != "kill_switch" {
		r.Metrics.Intervention(ctx, "stop")
	}
	return r.end(ctx, t, v1alpha1.PhaseStopped, why)
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
		r.Metrics.TaskTokens(ctx, t.Status.Usage.Tokens, t.Spec.Budget.Tier, t.Spec.Template, t.Spec.PredictedClass)
	}
	return r.narrator().Post(ctx, t, target(t), narrate.Ended(t, phase, reason))
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
	return r.startRun(ctx, t, r.implementerSpec(t, FirstBrief(t, r.Nonce())), "initial")
}
