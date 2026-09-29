// SPDX-License-Identifier: Apache-2.0

// Package roomctrl reconciles Room CRs (S2): the log row and its first event,
// the finalizer that deletes the room's runs and seals the log, and status.
package roomctrl

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/runwatch"
	"github.com/Smana/agent-platform/internal/store"
)

const (
	// Finalizer holds a deleted Room until its runs have ended and its log is sealed.
	Finalizer = "agents.ogenki.io/room-log"
	resync    = 15 * time.Second // ruling P21
	// sealTimeout bounds how long a deleting room waits for its runs to end,
	// from its deletionTimestamp (Ruling AH).
	sealTimeout   = 2 * time.Minute
	runPoll       = 5 * time.Second
	conflictRetry = time.Second
)

// Store is the part of the log the reconciler writes and reads. Broker-origin
// events take the plain Append; sealing goes only through CloseRoom (Ruling Y).
type Store interface {
	EnsureRoom(ctx context.Context, r store.NewRoom) (bool, error)
	Append(ctx context.Context, d envelope.Draft) (envelope.Event, bool, error)
	Room(ctx context.Context, id string) (store.RoomState, error)
	CloseRoom(ctx context.Context, id, reason string) error
	PendingApprovals(ctx context.Context, roomID string) (int, error)
}

// runIndex is the one watcher method the reconciler reads; *runwatch.Watcher has it.
type runIndex interface {
	InRoom(room string) []runwatch.Run
}

// runEnds records why a run ended (P15); *runwatch.Events has it. Both are
// idempotent by key, so recording what the watch already recorded writes nothing.
type runEnds interface {
	Observe(ctx context.Context, r runwatch.Run) error
	ObserveDeleted(ctx context.Context, r runwatch.Run) error
}

// Reconciler keeps a Room's log row, first event and status in step with the CR.
type Reconciler struct {
	// Client lists AgentRuns as unstructured objects, which controller-runtime's
	// client reads from the API server, not its cache, unless told otherwise.
	Client client.Client
	Store  Store
	// Runs feeds the status projection; a deletion lists runs through Client.
	Runs runIndex
	// Ends is required: a deletion records every run's end before it seals.
	Ends runEnds
	// Observe, when set, feeds the rooms{phase} and rooms_last_event_timestamp_seconds gauges.
	Observe func(room string, st v1alpha1.RoomStatus, lastEventAt time.Time)
	// Now is the clock for the stall check and the seal timeout; nil means time.Now.
	Now func() time.Time
	// Log receives the seal-timeout warning; nil discards it.
	Log *slog.Logger
}

// Reconcile ensures the room's row and its seq-1 Open event, projects the log and
// the runs into status, and requeues every resync so a stall is noticed.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var room v1alpha1.Room
	if err := r.Client.Get(ctx, req.NamespacedName, &room); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !room.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &room)
	}
	if controllerutil.AddFinalizer(&room, Finalizer) {
		if err := r.Client.Update(ctx, &room); err != nil {
			return requeueOnConflict(fmt.Errorf("roomctrl: add the finalizer to room %s: %w", room.Name, err))
		}
	}
	// Once status has seen the log, the row exists or retention purged it: opening
	// again would resurrect a purged log with a fresh seq 1.
	opened := room.Status.LastSeq > 0 || room.Status.Phase == "Closed"
	if !opened {
		if err := r.open(ctx, &room); err != nil {
			return ctrl.Result{}, err
		}
	}
	st, err := r.Store.Room(ctx, room.Name)
	if opened && errors.Is(err, store.ErrNoRoom) {
		want := room.Status
		want.Phase, want.ObservedGeneration = "Closed", room.Generation
		return ctrl.Result{}, r.patchStatus(ctx, &room, want)
	}
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("roomctrl: read room %s: %w", room.Name, err)
	}
	pending, err := r.Store.PendingApprovals(ctx, room.Name)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("roomctrl: pending approvals of room %s: %w", room.Name, err)
	}
	want := v1alpha1.RoomStatus{Phase: Phase(st, r.Runs.InRoom(room.Name), pending, r.now()),
		LastSeq: st.LastSeq, Driver: st.Driver, DriverEpoch: st.DriverEpoch,
		PendingApprovals: clampInt32(pending), ObservedGeneration: room.Generation}
	if err := r.patchStatus(ctx, &room, want); err != nil {
		return ctrl.Result{}, err
	}
	if r.Observe != nil {
		r.Observe(room.Name, want, st.LastEventAt)
	}
	return ctrl.Result{RequeueAfter: resync}, nil
}

// open inserts the room's row and appends its seq-1 Open event, both idempotent:
// a new leader, or a retry after a lost status write, writes nothing twice.
func (r *Reconciler) open(ctx context.Context, room *v1alpha1.Room) error {
	retention, err := ParseRetention(room.Spec.Retention)
	if err != nil {
		return reconcile.TerminalError(err) // the spec, not the world, has to change
	}
	if _, err := r.Store.EnsureRoom(ctx, store.NewRoom{ID: room.Name, Driver: room.Spec.Driver, Retention: retention}); err != nil {
		return fmt.Errorf("roomctrl: ensure room %s: %w", room.Name, err)
	}
	if _, _, err := r.Store.Append(ctx, envelope.Draft{RoomID: room.Name,
		Actor: envelope.Actor{Kind: envelope.ActorSystem, ID: "system:room-broker"}, Type: envelope.StateChanged,
		Origin: envelope.OriginBroker, OriginClient: "broker:room", OriginSeq: 1,
		Payload: envelope.StatePayload("room_phase", map[string]any{"phase": "Open", "owner": room.Spec.Owner,
			"driver": room.Spec.Driver, "dataClass": room.Spec.DataClass})}); err != nil && !errors.Is(err, store.ErrSealed) {
		return fmt.Errorf("roomctrl: open room %s: %w", room.Name, err)
	}
	return nil
}

// patchStatus writes want when it differs. A merge patch of the status
// subresource cannot conflict with a spec or finalizer write.
func (r *Reconciler) patchStatus(ctx context.Context, room *v1alpha1.Room, want v1alpha1.RoomStatus) error {
	if room.Status == want {
		return nil
	}
	base := room.DeepCopy()
	room.Status = want
	if err := r.Client.Status().Patch(ctx, room, client.MergeFrom(base)); err != nil {
		return fmt.Errorf("roomctrl: status of room %s: %w", room.Name, err)
	}
	return nil
}

// finalize deletes the room's runs, waits for them to end, records why each
// ended, seals the log (its retention clock starts), then releases the CR (§1,
// Ruling AH). A run still live sealTimeout after the deletion does not hold the
// seal: it is recorded as deleted, and a warning names it.
func (r *Reconciler) finalize(ctx context.Context, room *v1alpha1.Room) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(room, Finalizer) {
		return ctrl.Result{}, nil
	}
	ended, live, err := r.deleteRuns(ctx, room.Name)
	if err != nil {
		return ctrl.Result{}, err
	}
	now, deadline := r.now(), room.DeletionTimestamp.Add(sealTimeout)
	if len(live) > 0 && now.Before(deadline) {
		return ctrl.Result{RequeueAfter: min(runPoll, deadline.Sub(now))}, nil
	}
	if len(live) > 0 {
		r.log().WarnContext(ctx, "sealing a deleted room while runs are still live", "room", room.Name,
			"runs", ids(live), "timeout", sealTimeout)
	}
	if err := r.recordEnds(ctx, room.Name, ended, live); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.Store.CloseRoom(ctx, room.Name, "room deleted"); err != nil && !errors.Is(err, store.ErrNoRoom) {
		return ctrl.Result{}, fmt.Errorf("roomctrl: seal room %s: %w", room.Name, err)
	}
	controllerutil.RemoveFinalizer(room, Finalizer)
	if err := r.Client.Update(ctx, room); err != nil {
		return requeueOnConflict(fmt.Errorf("roomctrl: release room %s: %w", room.Name, err))
	}
	return ctrl.Result{}, nil
}

// deleteRuns lists the room's AgentRuns from the API rather than the watcher,
// which a new leader or a just-created run can find behind (review I1), deletes
// those not already deleting, and splits them into terminal and live.
func (r *Reconciler) deleteRuns(ctx context.Context, room string) (ended, live []runwatch.Run, err error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(runwatch.GVK().GroupVersion().WithKind(runwatch.GVK().Kind + "List"))
	if err := r.Client.List(ctx, list, client.InNamespace(runwatch.Namespace)); err != nil {
		return nil, nil, fmt.Errorf("roomctrl: list the runs of room %s: %w", room, err)
	}
	for i := range list.Items {
		u := &list.Items[i]
		run, ok := runwatch.FromUnstructured(u)
		if !ok || run.Room != room {
			continue
		}
		if u.GetDeletionTimestamp() == nil {
			if err := r.Client.Delete(ctx, u); client.IgnoreNotFound(err) != nil {
				return nil, nil, fmt.Errorf("roomctrl: delete run %s of room %s: %w", run.ID, room, err)
			}
		}
		if runwatch.Terminal(run.Phase) {
			ended = append(ended, run)
		} else {
			live = append(live, run)
		}
	}
	return ended, live, nil
}

// recordEnds writes why each run ended before the seal would refuse it (P15):
// the listed terminal runs, the listed runs still live at the timeout (deleted),
// and runs gone from the API that the watcher still remembers.
func (r *Reconciler) recordEnds(ctx context.Context, room string, ended, live []runwatch.Run) error {
	listed := map[string]bool{}
	for _, run := range ended {
		listed[run.ID] = true
		if err := r.Ends.Observe(ctx, run); err != nil {
			return fmt.Errorf("roomctrl: record the end of run %s: %w", run.ID, err)
		}
	}
	for _, run := range live {
		listed[run.ID] = true
		if err := r.Ends.ObserveDeleted(ctx, run); err != nil {
			return fmt.Errorf("roomctrl: record the deletion of run %s: %w", run.ID, err)
		}
	}
	for _, run := range r.Runs.InRoom(room) {
		if listed[run.ID] {
			continue
		}
		record := r.Ends.ObserveDeleted
		if runwatch.Terminal(run.Phase) {
			record = r.Ends.Observe
		}
		if err := record(ctx, run); err != nil {
			return fmt.Errorf("roomctrl: record the end of run %s: %w", run.ID, err)
		}
	}
	return nil
}

// requeueOnConflict turns a stale write into a quiet retry: the watch delivers the
// newer object, and a conflict is not a failure worth logging.
func requeueOnConflict(err error) (ctrl.Result, error) {
	if apierrors.IsConflict(err) {
		return ctrl.Result{RequeueAfter: conflictRetry}, nil
	}
	return ctrl.Result{}, err
}

func ids(runs []runwatch.Run) []string {
	out := make([]string, 0, len(runs))
	for _, r := range runs {
		out = append(out, r.ID)
	}
	return out
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Reconciler) log() *slog.Logger {
	if r.Log != nil {
		return r.Log
	}
	return slog.New(slog.DiscardHandler)
}

// SetupWithManager registers the reconciler for Rooms. controller-runtime runs one
// reconcile at a time by default, which bounds the store's load.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&v1alpha1.Room{}).Complete(r)
}
