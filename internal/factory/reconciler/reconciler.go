// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	roomv1 "github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/factory/api"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/fluxwatch"
	"github.com/Smana/agent-platform/internal/factory/fmetrics"
	"github.com/Smana/agent-platform/internal/factory/forge"
	"github.com/Smana/agent-platform/internal/factory/killswitch"
	"github.com/Smana/agent-platform/internal/factory/meter"
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
// from afterSeq with its resume cursor, its current seq, the room's queue, task_state messages and
// the task's facts.
type RoomLog interface {
	EventsSince(ctx context.Context, room string, afterSeq int64, keep func(envelope.Event) bool) ([]envelope.Event, int64, error)
	LastSeq(ctx context.Context, room string) (int64, error)
	Enqueue(ctx context.Context, room, stream, text string, clientSeq int64) error
	Queue(ctx context.Context, room string) ([]rooms.Queued, error)
	Consume(ctx context.Context, room string, refs []int64, runID string) error
	TaskState(ctx context.Context, room, text string, clientSeq int64) error
	TaskFacts(ctx context.Context, room string, f envelope.TaskFacts, clientSeq int64) error
}

// TaskForge is the part of the forge the reconciler uses, as the factory App.
type TaskForge interface {
	RecentComments(ctx context.Context, number int) ([]forge.Comment, error)
	Comment(ctx context.Context, number int, body string) error
	AddLabels(ctx context.Context, number int, labels ...string) error
	PullRequestForBranch(ctx context.Context, branch string) (int, error)
	PullRequest(ctx context.Context, number int) (forge.PR, error)
	PullRequestCommits(ctx context.Context, number int) ([]forge.Commit, error)
	BranchActivity(ctx context.Context, branch string) ([]forge.Push, error)
	ClosePR(ctx context.Context, number int) error
	RemoveLabel(ctx context.Context, number int, label string) error
	DefaultBranchFile(ctx context.Context, path string) (text, sha string, found bool, err error)
	Issue(ctx context.Context, number int) (forge.Issue, error)
	CreateIssue(ctx context.Context, title, body string, labels []string) (forge.Issue, error)
	IssuesLabelled(ctx context.Context, label string) ([]forge.Issue, error)
	IssuesLabelledSince(ctx context.Context, label string, since time.Time) ([]forge.Issue, error)
	AddSubIssue(ctx context.Context, parent int, childID int64) error
	CloseIssue(ctx context.Context, number int) error
}

// metrics is the part of fmetrics.Set the reconciler records.
type metrics interface {
	TimeToPR(ctx context.Context, d time.Duration, source, tier, template string)
	PROpened(ctx context.Context)
	PROutcome(ctx context.Context, class, outcome string)
	MonitorVerdict(ctx context.Context, class, verdict string)
	MonitorVerification(ctx context.Context, class, verdict string)
	HumanWait(ctx context.Context, phase string, d time.Duration)
	TaskTokens(ctx context.Context, tokens int64, tier, template, predictedClass string)
	Intervention(ctx context.Context, kind string)
	TierFit(ctx context.Context, classifier, tier, fit string, control bool)
	Revoked(ctx context.Context, reason string)
	AdmissionRefused(ctx context.Context, reason string)
	PrincipalShadow(ctx context.Context, principal string)
	RunRevoked(ctx context.Context, reason, limit string)
	RunBudgetFallback(ctx context.Context, scope, why string)
	TraceExportAbandoned(ctx context.Context)
	ClassMismatch(ctx context.Context, predicted, matched string)
	MergeDecision(ctx context.Context, class, verdict, reason string)
	Resumed(ctx context.Context, reason string)
	LoopRun(ctx context.Context, class, end string)
	RoomClosedAtCap(ctx context.Context, reason string)
	SteeringRead(ctx context.Context, class, read string)
	ScoperRun(ctx context.Context, class, outcome string)
	ScopeBounce(ctx context.Context, class, exit string)
	Split(ctx context.Context, class, outcome string)
	SplitChild(ctx context.Context, class, outcome string)
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
// /factory retry, the end of its pull request, or caps.escalatedHours (#61), and each poll costs
// GitHub calls the whole factory shares.
const escalatedPoll = 5 * time.Minute

// Reconciler is the Task state machine (§4). One reconcile per task every poll interval and on
// every change of one of its AgentRuns; one worker, so the caps are counted without a race
// between two tasks starting at once. Run ids are derived from the task (R48), never random.
type Reconciler struct {
	Client client.Client
	// Ledger reads and writes R50's day ledgers live, as the API and the meter do: the cached
	// client watches one ConfigMap, the stop object. Queued reserves its runs there (#78).
	Ledger    client.Client
	Namespace string
	Cfg       *config.Config
	Forge     TaskForge
	Merger    forge.Merger // the merger App: checks, merges and reverts (R16)
	// Flux reads Flux's objects on the factory's cluster for the post-merge monitor (MC7). nil:
	// the Flux signal reads inconclusive.
	Flux interface {
		Source(ctx context.Context, ns, name string) (fluxwatch.Source, error)
		NotReadySince(ctx context.Context, ns, name string, since, until time.Time) ([]string, error)
	}
	// Clients is v2 D8: a repository's two connections, the factory App's and the merger's, each
	// bound to that repository. Set, every reconcile binds its task's repository's pair, and a
	// task on a repository it does not know is refused before any call; nil, Forge and Merger
	// serve every task (one repository).
	Clients func(repository string) (TaskForge, forge.Merger, bool)
	// Owned is v2 D7's fence: the merge gate hands a repository this factory does not own to a
	// human. nil owns nothing.
	Owned   func(repository string) bool
	Runs    RunClient
	Rooms   RoomLog
	Triage  triage.Triager
	Metrics metrics
	Now     func() time.Time
	Nonce   func() string
	Log     *slog.Logger
	Trace   tracing.Sink // nil: tracing off (R46)
	// Costs is the run meter's per-run USD costs, which a task capped in USD sums (OD-6 (b));
	// nil: every task is held to TaskTokens.
	Costs costReader
	// CanWrite is v2 D4's GitHub check, as the label intake's: a /factory retry needs its
	// maintainer to hold write or higher on the task's repository. nil: nobody writes.
	CanWrite func(ctx context.Context, repository, login string) (bool, error)

	oldBroker sync.Once // the broker serves no task route: said once per process
	ledger    mergeLedger
	mon       monitorCache
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
	// v2 D10 and spend per identity SI4: the pass's series are the task's repository's and its payer's.
	ctx, err := r.bind(fmetrics.WithPrincipal(fmetrics.WithRepository(ctx, t.Spec.Repository), r.payer(&t)), &t)
	if err != nil {
		return ctrl.Result{}, err
	}
	ended := v1alpha1.TerminalPhase(t.Status.Phase)
	// A requested revert, an open revert PR and a merge under watch are the three exceptions to
	// leaving a terminal task alone (§6.4, MC7): the factory owes the revert its watch, a
	// maintainer's factory/revert its action, and a monitored merge its verdict, even after the
	// task itself has ended.
	watchWait := r.revertable(&t) || r.revertPending(&t) || r.monitorDue(&t)
	var closeDue bool
	var closeWait time.Duration
	if ended {
		var rerr error
		if closeDue, _, closeWait, rerr = r.roomState(ctx, &t); rerr != nil {
			return ctrl.Result{}, rerr
		}
	}
	if ended && !watchWait && len(t.Status.Outbox) == 0 && !r.spanDue(&t) && !r.settling(&t) && !r.factsDue(&t) && !closeDue {
		return ctrl.Result{RequeueAfter: closeWait}, nil // closeWait: the revert window still open (#56)
	}
	before := t.Status.DeepCopy()
	if t.Status.Phase == "" {
		r.to(&t, v1alpha1.PhaseReceived, "")
	}
	fx := &effects{}
	switch {
	case !ended, watchWait:
		err = r.step(context.WithValue(ctx, effectsKey{}, fx), &t)
	case r.settling(&t):
		err = r.settle(context.WithValue(ctx, effectsKey{}, fx), &t)
	}
	if err == nil && ended && watchWait && r.settling(&t) {
		// A task kept alive by a watch still settles its usage: the switch above took the watch.
		err = r.settle(context.WithValue(ctx, effectsKey{}, fx), &t)
	}
	if !equality.Semantic.DeepEqual(*before, t.Status) {
		if uerr := r.Client.Status().Update(ctx, &t); uerr != nil {
			r.mon.forget(t.Name) // the pass was not recorded: its retry must not wait out the throttle
			return ctrl.Result{}, errors.Join(err, uerr)
		}
		for _, f := range fx.after {
			f(ctx)
		}
		r.waited(ctx, before, &t.Status)
	}
	// Only a written outbox is posted (review M-a), only written facts reach the room, and only a
	// written end exports the task's span: a write that conflicts posts and exports nothing, and its
	// replay may take another path. The second write records what was posted and exported. Facts
	// are advisory: a failed post is returned for a retry, and no phase waits on it.
	if len(t.Status.Outbox) > 0 || r.spanDue(&t) || r.factsDue(&t) {
		queued := t.Status.DeepCopy()
		err = errors.Join(err, r.drain(ctx, &t))
		err = errors.Join(err, r.postFacts(ctx, &t))
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
		// #56: the room closes once everything the task owes it has landed. Re-evaluated on the
		// state this pass left, so the pass that posts the last fact closes it.
		var closeLeft time.Duration
		if err == nil {
			if due, capped, wait, serr := r.roomState(ctx, &t); serr != nil {
				err = serr
			} else if due {
				var closed bool
				if closed, err = r.closeRoom(ctx, &t, capped != ""); closed && capped != "" {
					r.log().Info("room closed at the cap", "task.id", t.Name, "reason", capped)
					r.Metrics.RoomClosedAtCap(ctx, capped)
				}
			} else {
				closeLeft = wait
			}
		}
		// The revert watch polls: a revert PR merges, closes or stalls on GitHub's clock, not ours.
		if err == nil && (r.revertable(&t) || r.revertPending(&t)) {
			return ctrl.Result{RequeueAfter: r.Cfg.Poll.Tasks.Duration}, nil
		}
		if err == nil && r.settling(&t) {
			wait := r.settleLeft(&t) // for the settle, at its window, unless a merge under watch is due sooner
			if r.monitorDue(&t) {
				wait = min(wait, monitorEvery)
			}
			return ctrl.Result{RequeueAfter: wait}, nil
		}
		if err == nil && r.monitorDue(&t) && (closeLeft <= 0 || closeLeft > monitorEvery) {
			return ctrl.Result{RequeueAfter: monitorEvery}, nil // a merge under watch is read on its own clock
		}
		return ctrl.Result{RequeueAfter: closeLeft}, err // closeLeft: the revert window or a driving person (#56)
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
	case v1alpha1.PhaseSplit:
		return r.splitting(ctx, t)
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
		if r.revertable(t) {
			return r.revert(ctx, t, "revert_requested")
		}
		return r.monitor(ctx, t) // reached only when monitorDue
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

// payer is the key the task's costs and yield count for (spend per identity SI1): the person its
// next run is charged to, else the fleet.
func (r *Reconciler) payer(t *v1alpha1.Task) string {
	return runs.TaskPayer(t, r.Cfg.Budgets.PerPerson.Enabled)
}

// waited adds a written exit from AwaitingHuman or Escalated to the payer's waits (SI8): the time
// from the phase's start to the next phase's, both as the status records them.
func (r *Reconciler) waited(ctx context.Context, before, after *v1alpha1.TaskStatus) {
	if before.Phase == after.Phase || before.PhaseSince == nil || after.PhaseSince == nil {
		return
	}
	if before.Phase == v1alpha1.PhaseAwaitingHuman || before.Phase == v1alpha1.PhaseEscalated {
		r.Metrics.HumanWait(ctx, before.Phase, after.PhaseSince.Sub(before.PhaseSince.Time))
	}
}

func (r *Reconciler) to(t *v1alpha1.Task, phase, reason string) {
	if t.Status.Phase != phase {
		now := metav1.NewTime(r.Now())
		t.Status.PhaseSince = &now
	}
	t.Status.Phase, t.Status.Reason = phase, reason
}

// repo is the config entry of the task's repository (v2 D1).
func (r *Reconciler) repo(t *v1alpha1.Task) *config.Repository {
	return r.Cfg.RepositoryFor(t.Spec.Repository)
}

func (r *Reconciler) narrator(ctx context.Context) narrate.Narrator {
	return narrate.Narrator{Forge: r.gh(ctx), Login: r.Cfg.FactoryLogin}
}

// boundKey carries the reconciled task's connections (v2 D8): every GitHub call of the pass reads
// them from its context, so none can reach another repository's.
type boundKey struct{}

type bound struct {
	forge  TaskForge
	merger forge.Merger
}

// bind puts t's repository's connections on ctx; an unknown repository is an error, so nothing
// acts for it.
func (r *Reconciler) bind(ctx context.Context, t *v1alpha1.Task) (context.Context, error) {
	if r.Clients == nil {
		return ctx, nil
	}
	f, m, ok := r.Clients(t.Spec.Repository)
	if !ok {
		return ctx, fmt.Errorf("task %s: no GitHub connection for repository %q (v2 D8)", t.Name, t.Spec.Repository)
	}
	return context.WithValue(ctx, boundKey{}, bound{forge: f, merger: m}), nil
}

// gh is the factory App on the task's repository, merger the merger App there.
func (r *Reconciler) gh(ctx context.Context) TaskForge {
	if b, ok := ctx.Value(boundKey{}).(bound); ok {
		return b.forge
	}
	return r.Forge
}

func (r *Reconciler) merger(ctx context.Context) forge.Merger {
	if b, ok := ctx.Value(boundKey{}).(bound); ok {
		return b.merger
	}
	return r.Merger
}

// onRepo: t is a task of rc's repository. One entry owns every task, as before the list.
func (r *Reconciler) onRepo(t *v1alpha1.Task, rc *config.Repository) bool {
	return len(r.Cfg.Repos()) == 1 || strings.EqualFold(t.Spec.Repository, rc.Name)
}

func stopReasons() map[string]string {
	return map[string]string{"true": "stopped_by_annotation", "label": "stopped_by_label", "superseded": "superseded", StopFamilyDropped: "family_dropped"}
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
// conflicting status write is still the task's, and still stopped. The claims' last usage is
// copied into the records first: once deleted, the settle can no longer read it (#55).
func (r *Reconciler) stop(ctx context.Context, t *v1alpha1.Task, why string) error {
	all, err := r.Runs.List(ctx)
	if err != nil {
		return err
	}
	if len(t.Status.Runs) > 0 {
		absorb(t, all)
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
				r.Metrics.Revoked(fmetrics.WithPrincipal(ctx, run.Payer()), "manual") // as the ledger charged it
			}
		}
		if err := r.deleteRun(ctx, run.ID); err != nil {
			return err
		}
	}
	if why != "kill_switch" {
		record(ctx, func(ctx context.Context) { r.Metrics.Intervention(ctx, "stop") })
	}
	exit := ""
	if why == "superseded" {
		exit = "relabel" // SC8 (a): the author edited, a maintainer re-labelled
	}
	r.leaveBounce(ctx, t, exit)
	splitExit := SplitDeclined // a maintainer's stop declines the split
	if why == "superseded" {
		splitExit = SplitSuperseded // SL4 (c): the author edited, a maintainer re-labelled
	}
	r.leaveSplit(ctx, t, splitExit)
	if sp := t.Status.Split; t.Status.Phase == v1alpha1.PhaseSplit && sp != nil { // its open parts stop with it (SL12)
		newest, _, err := r.family(ctx, t)
		if err != nil {
			return err
		}
		if err := r.stopOpenParts(ctx, sp, newest); err != nil {
			return err
		}
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
		if narrate.Closing(o.Key) && r.settling(t) {
			break // its token total is final only once the usage settles (#42); settle requeues
		}
		if err := r.narrator(ctx).Post(ctx, t, o.Number, narrate.Event{Key: o.Key, Body: o.Body}); err != nil {
			return err
		}
		t.Status.Outbox = t.Status.Outbox[1:]
	}
	if len(t.Status.Outbox) == 0 {
		t.Status.Outbox = nil
	}
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
// task.final line the moment the task first ends. The reservations no claim holds any more are
// released here (#55): every ending path, and every replay of one, passes through it.
func (r *Reconciler) end(ctx context.Context, t *v1alpha1.Task, phase, reason string) error {
	r.finish(ctx, t, phase, reason, narrate.Ended(t, phase, reason, r.repo(t).Maintainers...))
	return nil
}

// finish is the ending every task shares: its unheld reservations released, the move, a terminal
// approved parent's parts counted (SL14), the narration and the outcome.
func (r *Reconciler) finish(ctx context.Context, t *v1alpha1.Task, phase, reason string, e narrate.Event) {
	r.releaseUnheld(ctx, t)
	r.to(t, phase, reason)
	if v1alpha1.TerminalPhase(phase) {
		r.countFamily(ctx, t) // an approved parent stopped or expired: its landed parts count (SL14)
	}
	narrateLater(t, e)
	r.outcome(ctx, t)
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
	blind := slices.ContainsFunc(t.Status.Runs, func(rec v1alpha1.RunRecord) bool { return rec.Tokens == 0 })
	for i := range t.Status.Outbox {
		if o := &t.Status.Outbox[i]; narrate.Closing(o.Key) { // the closing comment waited for this total (#42)
			o.Body = narrate.Retotal(o.Body, t.Status.Usage.Tokens, blind)
		}
	}
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
	if t.Spec.Source.Kind == "adopt" {
		return "", nil // R39: Renovate's own pull request starts no run: the daily task cap is not its to spend
	}
	today, err := r.countTasks(ctx, func(o *v1alpha1.Task) bool {
		return o.Name != t.Name && o.Spec.Source.Kind != "adopt" && o.Status.Phase != "" && o.Status.Phase != v1alpha1.PhaseRejected &&
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
	waitsForIssue := t.Spec.Source.Kind == "runlore" || (t.Spec.Source.Kind == "api" && strings.HasPrefix(t.Spec.Text, "# "))
	if waitsForIssue && t.Spec.Issue == 0 && r.Now().Sub(t.CreationTimestamp.Time) < 2*time.Minute {
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
	if t.Spec.Source.Kind == "adopt" {
		return r.receivedAdopted(ctx, t)
	}
	if t.Spec.Parent != nil {
		return r.receivedChild(ctx, t) // a part of a split: its parent's spec, no classifier (SL5)
	}
	d, err := r.Triage.Triage(ctx, t)
	if err != nil {
		return err
	}
	t.Spec.Template, t.Spec.PredictedClass, t.Spec.DataClass = d.Template, d.PredictedClass, d.DataClass
	t.Spec.Budget = triage.Ceiling(d.Budget, t.Spec.MaxMicroUSD) // F4: a task without a ceiling keeps its class budget, byte for byte
	t.Spec.Loop = nil
	if p := r.Cfg.LoopFor(d.PredictedClass); p != nil {
		t.Spec.Loop = &v1alpha1.Loop{StallSteps: p.StallSteps, Continuations: p.Continuations, JudgeModel: p.JudgeModel}
	}
	t.Spec.Search = r.Cfg.SearchFor(d.PredictedClass)
	if err := r.scopeAtTriage(ctx, t); err != nil {
		return err // a note that cannot be read is retried, never read as none (SC4)
	}
	r.splitAtTriage(t, d.Classification)
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
	var members []roomv1.Member
	if p := t.Sponsored(); p != "" { // R8, R13: the principal reads its own task's room, as a watcher
		members = append(members, roomv1.Member{Principal: p, Role: "watcher"})
	}
	err := rooms.Ensure(ctx, r.Client, r.Namespace, t.Name, t.Spec.DataClass, t.Spec.Repository, members...)
	if errors.Is(err, rooms.ErrForeignRoom) {
		return r.end(ctx, t, v1alpha1.PhaseEscalated, "foreign_room")
	}
	if apierrors.IsInvalid(err) && len(members) > 0 {
		// an old Rooms CRD refuses a system: member: retrying cannot help, an operator must act
		return r.end(ctx, t, v1alpha1.PhaseEscalated, "rooms_crd_outdated")
	}
	if err != nil {
		return err
	}
	t.Status.RoomRef = t.Name
	r.to(t, v1alpha1.PhaseQueued, "")
	return nil
}

// costReader is the run meter's hand-off of each run's cost (meter.Costs).
type costReader interface {
	Get(id string) (meter.Cost, bool)
}

// runFits: the task's cap still holds one whole run (owner, 2026-10-07). A run may spend up to
// its run cap before the meter revokes it, so admitting on "under the cap" let a task end 1.5 M
// past it; a resume follows the same rule. In USD while usdCapped, else in tokens.
func runFits(t *v1alpha1.Task) bool {
	b := t.Spec.Budget
	if usdCapped(t) {
		return b.TaskMicroUSD-t.Status.Usage.CostMicroUSD >= b.RunMicroUSD
	}
	return b.TaskTokens <= 0 || b.TaskTokens-t.Status.Usage.Tokens >= b.RunTokens
}

// usdCapped: the task is held to TaskMicroUSD rather than TaskTokens (OD-6 (b)), while every run
// it holds was priced. A run the meter could not price, or one with tokens and no cost (a task
// begun before the meter priced runs), holds it to TaskTokens: never a cost counted as $0.
func usdCapped(t *v1alpha1.Task) bool {
	if t.Spec.Budget.TaskMicroUSD <= 0 {
		return false
	}
	for _, rec := range t.Status.Runs {
		if rec.Unpriced != "" || (rec.Tokens > 0 && rec.CostMicroUSD == 0) {
			return false
		}
	}
	return true
}

// taskLimit names the task cap runFits checks, for the revocations metric.
func taskLimit(t *v1alpha1.Task) string {
	if usdCapped(t) {
		return "task-usd"
	}
	return "task-tokens"
}

// price takes each run's cost from the meter into the task's records: a record keeps its last
// cost once its claim is gone, as it keeps its tokens (R49). The first run the meter cannot
// price, on a task with a USD cap, is said once: the task falls back to TaskTokens.
func (r *Reconciler) price(ctx context.Context, t *v1alpha1.Task) {
	if r.Costs == nil {
		return
	}
	before := unpriced(t)
	var sum int64
	for i := range t.Status.Runs {
		rec := &t.Status.Runs[i]
		if c, ok := r.Costs.Get(rec.ID); ok {
			served(&rec.Provenance, c.Served)
			rec.CostMicroUSD = max(rec.CostMicroUSD, c.MicroUSD)
			if rec.Unpriced == "" {
				rec.Unpriced = c.Why
			}
		}
		sum += rec.CostMicroUSD
	}
	t.Status.Usage.CostMicroUSD = sum
	if why := unpriced(t); why != "" && before == "" && t.Spec.Budget.TaskMicroUSD > 0 {
		r.log().Warn("task capped in tokens: a run's cost cannot be priced", "task.id", t.Name, "why", why,
			"cap", t.Spec.Budget.TaskTokens)
		record(ctx, func(ctx context.Context) { r.Metrics.RunBudgetFallback(ctx, "task", why) })
	}
}

func unpriced(t *v1alpha1.Task) string {
	for _, rec := range t.Status.Runs {
		if rec.Unpriced != "" {
			return rec.Unpriced
		}
	}
	return ""
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
		if why == "waiting_room_busy" || why == "waiting_human_driver" { // the room is held, not the factory's own caps (N3)
			narrateLater(t, narrate.Held(t, why, r.Cfg.RoomsURL))
		}
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
	// A CI fix starts from the branch head: one a human pushed would ride the fix to a merge.
	if late && t.Status.NextRole == "" && nextTrigger(t) == "ci" {
		if handed, err := r.humanCommit(ctx, t, pr.HeadSHA); err != nil || handed {
			return err
		}
	}
	if !runFits(t) {
		limit := taskLimit(t)
		if r.Cfg.Budgets.EnforceTask || t.Sponsored() != "" { // F4: an api task's ceiling is never a shadow
			record(ctx, func(ctx context.Context) { r.Metrics.RunRevoked(ctx, "budget-task", limit) })
			return r.end(ctx, t, v1alpha1.PhaseEscalated, "budget-task") // no new run past the task cap (C5)
		}
		// R3: shadow first, a week of numbers before the flag flips.
		record(ctx, func(ctx context.Context) { r.Metrics.RunRevoked(ctx, "budget-task-shadow", limit) })
		r.log().Info("task cap has no room for a whole run (shadow)", "task.id", t.Name, "limit", limit,
			"used", t.Status.Usage.Tokens, "run", t.Spec.Budget.RunTokens, "cap", t.Spec.Budget.TaskTokens,
			"used_micro_usd", t.Status.Usage.CostMicroUSD, "cap_micro_usd", t.Spec.Budget.TaskMicroUSD)
	}
	// R34: the factory's own day, checked before every run it starts, not only by the meter.
	// M-8: an api task's principal that left the config would run in shadow: refuse it first.
	if p := t.Sponsored(); p != "" && r.Cfg.PrincipalBy(p) == nil {
		return r.end(ctx, t, v1alpha1.PhaseEscalated, "principal_unknown")
	}
	limit, sponsored := r.Cfg.Budgets.DailyMicroUSD(runs.PrincipalFactory), r.sponsored(t)
	if limit > 0 {
		over, err := r.reserveFactoryDay(ctx, t, limit)
		switch {
		case errors.Is(err, api.ErrPersonOverBudget): // D4/D5: the person's day, not the fleet's: refused, not queued
			record(ctx, func(ctx context.Context) { r.Metrics.Revoked(ctx, "budget-person") })
			return r.end(ctx, t, v1alpha1.PhaseEscalated, "budget-person")
		case errors.Is(err, api.ErrOverBudget):
			t.Status.Reason = "waiting_daily_budget" // no new factory run until 00:00 UTC
			return nil
		case errors.Is(err, api.ErrMeterBlind): // an enforced cap the meter cannot feed holds (#54)
			t.Status.Reason = "waiting_meter_blind"
			return nil
		case err != nil && (r.Cfg.Budgets.EnforcePrincipal || sponsored):
			return err
		case err != nil: // a shadow cap counts; it never holds a run
			r.log().Warn("daily cap unreserved (shadow)", "task.id", t.Name, "err", err)
		case over != "":
			record(ctx, func(ctx context.Context) { r.Metrics.PrincipalShadow(ctx, over) })
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
		// SC3: the note once, after the snapshot, which stays the room's first task_state.
		if t.Spec.Steering != nil {
			if err := narrate.Room(ctx, r.Rooms, t, "steering", SteeringMessage(t),
				func(ctx context.Context) error { return r.Client.Status().Update(ctx, t) }); err != nil {
				return err
			}
		}
	}
	if late && t.Status.NextRole != "" {
		return r.startVerifier(ctx, t, pr)
	}
	// R38: a task of a [triager] template always runs the triager, retry included: no implementer
	// run ever starts from an internal-origin task. A missing template is no team, so it is not a
	// triager one either: the config's validation cannot guard a name removed after a task was
	// triaged.
	if r.Cfg.Investigates(t.Spec.Template) {
		text, ok, err := r.resumeBrief(ctx, t)
		if err != nil {
			return err
		}
		if !ok {
			text = resumed(t, nextTrigger(t)) + TriagerBrief(t, r.Nonce())
		}
		s := r.implementerSpec(t, text)
		s.Role = "triager"
		return r.startRun(ctx, t, s, nextTrigger(t))
	}
	if scoperDue(t) {
		return r.startScoper(ctx, t)
	}
	s, refs, trigger, err := r.nextImplementer(ctx, t)
	if err != nil {
		return err
	}
	if err := r.startRun(ctx, t, s, trigger); err != nil {
		return err
	}
	if t.Status.Phase == v1alpha1.PhaseEscalated { // the cluster refused the loop claim: no run exists
		return nil
	}
	// Consumed once the run exists, and only what its brief quoted (F-A). A failure only means
	// the next brief repeats them.
	if err := r.Rooms.Consume(ctx, t.Status.RoomRef, refs, current(t).ID); err != nil {
		r.log().Warn("queue consume failed", "task.id", t.Name, "run.id", current(t).ID, "err", err)
	}
	return nil
}
