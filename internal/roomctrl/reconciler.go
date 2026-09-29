// SPDX-License-Identifier: Apache-2.0

// Package roomctrl reconciles Room CRs (S2): the log row and its first event,
// the finalizer that deletes the room's runs and seals the log, and status.
package roomctrl

import (
	"context"
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/runwatch"
	"github.com/Smana/agent-platform/internal/store"
)

const (
	// Finalizer holds a deleted Room until its runs are deleted and its log sealed.
	Finalizer = "agents.ogenki.io/room-log"
	resync    = 15 * time.Second // ruling P21
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

// Reconciler keeps a Room's log row, first event and status in step with the CR.
type Reconciler struct {
	Client client.Client
	Store  Store
	Runs   runIndex
	// Observe, when set, feeds the rooms{phase} and rooms_last_event_timestamp_seconds gauges.
	Observe func(room string, st v1alpha1.RoomStatus, lastEventAt time.Time)
	// Now is the clock the stall check reads; nil means time.Now.
	Now func() time.Time
}

// Reconcile ensures the room's row and its seq-1 Open event, projects the log and
// the runs into status, and requeues every resync so a stall is noticed.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var room v1alpha1.Room
	if err := r.Client.Get(ctx, req.NamespacedName, &room); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !room.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalize(ctx, &room)
	}
	if controllerutil.AddFinalizer(&room, Finalizer) {
		if err := r.Client.Update(ctx, &room); err != nil {
			return ctrl.Result{}, fmt.Errorf("roomctrl: add the finalizer to room %s: %w", room.Name, err)
		}
	}
	retention, err := ParseRetention(room.Spec.Retention)
	if err != nil {
		return ctrl.Result{}, err
	}
	if _, err := r.Store.EnsureRoom(ctx, store.NewRoom{ID: room.Name, Driver: room.Spec.Driver, Retention: retention}); err != nil {
		return ctrl.Result{}, fmt.Errorf("roomctrl: ensure room %s: %w", room.Name, err)
	}
	// Idempotent by key: a new leader or a resync writes nothing twice.
	if _, _, err := r.Store.Append(ctx, envelope.Draft{RoomID: room.Name,
		Actor: envelope.Actor{Kind: envelope.ActorSystem, ID: "system:room-broker"}, Type: envelope.StateChanged,
		Origin: envelope.OriginBroker, OriginClient: "broker:room", OriginSeq: 1,
		Payload: envelope.StatePayload("room_phase", map[string]any{"phase": "Open", "owner": room.Spec.Owner,
			"driver": room.Spec.Driver, "dataClass": room.Spec.DataClass})}); err != nil && !errors.Is(err, store.ErrSealed) {
		return ctrl.Result{}, fmt.Errorf("roomctrl: open room %s: %w", room.Name, err)
	}
	st, err := r.Store.Room(ctx, room.Name)
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
	if room.Status != want {
		room.Status = want
		if err := r.Client.Status().Update(ctx, &room); err != nil {
			return ctrl.Result{}, fmt.Errorf("roomctrl: status of room %s: %w", room.Name, err)
		}
	}
	if r.Observe != nil {
		r.Observe(room.Name, want, st.LastEventAt)
	}
	return ctrl.Result{RequeueAfter: resync}, nil
}

func (r *Reconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

// finalize deletes the room's runs, seals the log (its retention clock starts),
// then releases the CR (§1). A room whose row never existed has nothing to seal.
func (r *Reconciler) finalize(ctx context.Context, room *v1alpha1.Room) error {
	if !controllerutil.ContainsFinalizer(room, Finalizer) {
		return nil
	}
	for _, run := range r.Runs.InRoom(room.Name) {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(runwatch.GVK())
		u.SetNamespace(runwatch.Namespace)
		u.SetName("xplane-run-" + run.ID)
		if err := r.Client.Delete(ctx, u); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("roomctrl: delete run %s of room %s: %w", run.ID, room.Name, err)
		}
	}
	if err := r.Store.CloseRoom(ctx, room.Name, "room deleted"); err != nil && !errors.Is(err, store.ErrNoRoom) {
		return fmt.Errorf("roomctrl: seal room %s: %w", room.Name, err)
	}
	controllerutil.RemoveFinalizer(room, Finalizer)
	if err := r.Client.Update(ctx, room); err != nil {
		return fmt.Errorf("roomctrl: release room %s: %w", room.Name, err)
	}
	return nil
}

// SetupWithManager registers the reconciler for Rooms. controller-runtime runs one
// reconcile at a time by default, which bounds the store's load.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&v1alpha1.Room{}).Complete(r)
}
