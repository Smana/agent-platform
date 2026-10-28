// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/bridge"
	"github.com/Smana/agent-platform/internal/brief"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/roomctrl"
	"github.com/Smana/agent-platform/internal/runrequest"
	"github.com/Smana/agent-platform/internal/store"
	"github.com/Smana/agent-platform/internal/wire"
)

// ForkedFrom annotates a forked Room with <source room>@<seq>; roomctrl owns it, since the
// Room's task status must skip the copied prefix.
const ForkedFrom = roomctrl.ForkedFrom

// maxNote bounds a fork's note, which its forked_from event carries.
const maxNote = 1 << 10

// fork branches the room at act.Seq into a new room the forker owns and drives
// (§5): the log prefix with its seqs, then state_changed{forked_from} with the
// commit at that seq, then, for a role, the new room's first run, on its own
// branch and the forker's token. Nothing is written to the source room, so a
// sealed one forks too, for its owners. A fork asks by default: its owner gets the
// attended profile, the source's ttl and fourEyes and the source's overrides but
// an allow, so a factory room's unattended policy never makes a human's own fork
// run unasked. act.Approvals "unattended" keeps the source's approvals as they
// are (a fork is never weaker than its source: an attended source stays
// attended). Anyone else gets, class by class, the stricter of the source's and
// a new room's (ruling M2), whatever they ask. The ack's result is {roomId, run?, runError?}: a run that
// fails leaves the fork made.
func (a *Actor) fork(ctx context.Context, p authn.Principal, owner bool, room *v1alpha1.Room, act Action, d envelope.Draft) (envelope.Event, json.RawMessage, string) {
	fail := func(reason string) (envelope.Event, json.RawMessage, string) { return envelope.Event{}, nil, reason }
	note := strings.TrimSpace(act.Note)
	if act.Approvals != "" && act.Approvals != "attended" && act.Approvals != "unattended" {
		return fail(rejectBadAction)
	}
	if act.Seq < 1 || len(note) > maxNote || (act.Role != "" && !a.validRun(room, act)) || !memberPrincipal.MatchString(p.ID) {
		return fail(rejectBadAction) // the owner must be one the Room CRD admits
	}
	if a.forkLimited(p.ID) {
		return fail(rejectRateLimited)
	}
	retention, err := roomctrl.ParseRetention(room.Spec.Retention)
	if err != nil || a.Rooms == nil {
		return fail(rejectLogUnavailable)
	}
	evs, err := a.Log.BriefSourcesThrough(ctx, room.Name, act.Seq)
	if err != nil {
		return fail(rejectLogUnavailable)
	}
	fields := map[string]any{}
	if commit := brief.LastCommit(evs); commit != "" {
		fields["commit"] = commit // the fork's first baseRef, and its PR's Forked-from line
	}
	if note != "" {
		if note, d.Redactions, err = a.redactText(ctx, note); err != nil {
			return fail(rejectLogUnavailable)
		}
		fields["note"] = note
	}
	id := runrequest.NewID()
	ev, err := a.Log.Fork(ctx, room.Name, act.Seq, store.NewRoom{ID: id, Driver: p.ID, Retention: retention}, d, fields)
	switch {
	case errors.Is(err, store.ErrBadSeq):
		return fail(rejectBadAction)
	case errors.Is(err, store.ErrForkTooLarge):
		return fail(rejectTooLarge)
	case err != nil:
		return fail(rejectLogUnavailable)
	}
	approvals := *room.Spec.Approvals.DeepCopy()
	switch {
	case !owner:
		// A new room's policy is the CRD's default: createRoom sets none.
		s := bridge.Stricter(wire.ApprovalPolicy{Profile: approvals.Profile, Overrides: approvals.Overrides}, wire.ApprovalPolicy{})
		approvals.Profile, approvals.Overrides = s.Profile, s.Overrides
	case act.Approvals != "unattended":
		// The owner's own fork asks: attended, keeping the source's ttl and fourEyes and every
		// override that does not loosen the gate.
		approvals.Profile, approvals.Overrides = "attended", nil
		for class, v := range room.Spec.Approvals.Overrides {
			if v != "allow" {
				if approvals.Overrides == nil {
					approvals.Overrides = map[string]string{}
				}
				approvals.Overrides[class] = v
			}
		}
	}
	child := &v1alpha1.Room{ObjectMeta: metav1.ObjectMeta{Name: id, Namespace: room.Namespace,
		Annotations: map[string]string{ForkedFrom: fmt.Sprintf("%s@%d", room.Name, act.Seq)}},
		Spec: v1alpha1.RoomSpec{Owner: p.ID, Driver: p.ID, DataClass: room.Spec.DataClass, Repository: room.Spec.Repository,
			Approvals: approvals, Retention: room.Spec.Retention}}
	if err := a.Rooms.Create(ctx, child); err != nil {
		if a.Logger != nil {
			a.Logger.WarnContext(ctx, "forked room not created", "room", id, "repository", child.Spec.Repository, "err", err)
		}
		// A row no Room projects is never closed, so retention would never purge it.
		_ = a.Log.CloseRoom(ctx, id, "fork failed: no Room")
		if apierrors.IsAlreadyExists(err) {
			return fail(rejectConflict)
		}
		return fail(rejectLogUnavailable)
	}
	result := map[string]any{"roomId": id}
	if act.Role != "" {
		fd := envelope.Draft{RoomID: id, Actor: d.Actor, Origin: d.Origin, OriginClient: d.OriginClient + ":fork", OriginSeq: d.OriginSeq}
		_, claim, why := a.startRun(ctx, p, child, Action{Role: act.Role, PRURL: act.PRURL, EgressProfiles: act.EgressProfiles}, fd)
		result["run"] = claim
		if why != "" {
			result["runError"] = why
		}
	}
	return ev, envelope.Must(result), ""
}
