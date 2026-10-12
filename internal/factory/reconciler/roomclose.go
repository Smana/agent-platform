// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"context"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	roomv1 "github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/runs"
)

// roomState says whether an ended task's room is to be closed now (#56), and when to look again
// if it is only waiting. The room's log takes the closing comment's facts and a revert's end, so
// it closes only once the outbox is drained, the usage settled, the facts posted and the revert
// window passed. It is a decision on live state, not a marker: a restart or a replay reaches the
// same answer, and a room already closed (or never made) is "not due".
func (r *Reconciler) roomState(ctx context.Context, t *v1alpha1.Task) (due bool, wait time.Duration, err error) {
	if !v1alpha1.TerminalPhase(t.Status.Phase) || t.Status.RoomRef == "" || len(t.Status.Outbox) > 0 ||
		r.settling(t) || r.revertable(t) || r.revertPending(t) || r.factsDue(t) {
		return false, 0, nil
	}
	if ref := t.Status.PullRequest; t.Status.Phase == v1alpha1.PhaseDone && ref != nil && ref.AutoMerged &&
		ref.RevertNumber == 0 && ref.MergedAt != nil {
		if left := ref.MergedAt.Add(r.Cfg.Merge.RevertWindow.Duration).Sub(r.Now()); left > 0 {
			return false, left, nil // a maintainer may still ask for the revert, which posts facts again
		}
	}
	var room roomv1.Room
	err = r.Client.Get(ctx, types.NamespacedName{Namespace: r.Namespace, Name: t.Status.RoomRef}, &room)
	switch {
	case apierrors.IsNotFound(err):
		return false, 0, nil
	case err != nil:
		return false, 0, fmt.Errorf("room %s: %w", t.Status.RoomRef, err)
	}
	// The factory closes only what it made: a room of the task's name that another owner made
	// is that owner's (rooms.Ensure refuses it the same way).
	return room.DeletionTimestamp == nil && room.Spec.Owner == runs.PrincipalFactory, 0, nil
}

// closeRoom deletes the room, which the room controller finalizes: it ends the room's runs and
// seals the log, and the seal starts the retention clock. NotFound is a room already closed.
func (r *Reconciler) closeRoom(ctx context.Context, t *v1alpha1.Task) error {
	room := &roomv1.Room{}
	room.Name, room.Namespace = t.Status.RoomRef, r.Namespace
	if err := r.Client.Delete(ctx, room); client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("close room %s: %w", room.Name, err)
	}
	return nil
}
