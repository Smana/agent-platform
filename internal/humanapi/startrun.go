// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"time"

	"github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/brief"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/roomctrl"
	"github.com/Smana/agent-platform/internal/runrequest"
)

// runRoles are the roles a human starts (§1); egressProfile a profile's name.
var (
	runRoles      = map[string]bool{"implementer": true, "reviewer": true, "triager": true}
	egressProfile = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

const (
	maxEgressProfiles = 8
	// pendingRunTTL is how long a requested run that has not joined holds the
	// room: long enough for the owner to apply a rendered claim by hand.
	pendingRunTTL = 10 * time.Minute
)

// busy reports whether the room has a run: a Running one, or one requested
// within pendingRunTTL that has not joined and that the watch does not know
// (review 4.4 I2). A run the watch knows is judged by its phase instead.
func (a *Actor) busy(ctx context.Context, room string) (bool, error) {
	if _, running := a.running(room); running {
		return true, nil // one Running run per room (D7)
	}
	pending, err := a.Log.PendingRuns(ctx, room, pendingRunTTL)
	if err != nil {
		return false, err
	}
	known := map[string]bool{}
	for _, r := range a.Runs.InRoom(room) {
		known[r.ID] = true
	}
	for _, id := range pending {
		if !known[id] {
			return true, nil
		}
	}
	return false, nil
}

// isRunRequest reports whether ev is a start_run's record.
func isRunRequest(ev envelope.Event) bool {
	var p struct {
		Kind string `json:"kind"`
	}
	return ev.Type == envelope.StateChanged && json.Unmarshal(ev.Payload, &p) == nil && p.Kind == "run_requested"
}

// validRun reports whether act names a role a human starts, egress profiles by
// name and never in an internal room (T5: the factory refuses them, and its 400
// would read as factory_unavailable), and a PR only for a reviewer and only of
// the room's repository.
func (a *Actor) validRun(room *v1alpha1.Room, act Action) bool {
	if !runRoles[act.Role] || len(act.EgressProfiles) > maxEgressProfiles ||
		(room.Spec.DataClass == "internal" && len(act.EgressProfiles) > 0) ||
		(act.PRURL != "" && (act.Role != "reviewer" || !a.prs.For(room.Spec.Repository).IsPR(act.PRURL))) {
		return false
	}
	for _, e := range act.EgressProfiles {
		if !egressProfile.MatchString(e) {
			return false
		}
	}
	return true
}

// startRun requests the room's next run (§1 The brief, ruling P24) and records
// it, with the queued messages its brief quoted, as state_changed{run_requested}.
// The ack's result is the rendered claim before SP3. A replayed clientSeq acks
// the record without asking again, and without the result.
func (a *Actor) startRun(ctx context.Context, p authn.Principal, room *v1alpha1.Room, act Action, d envelope.Draft) (envelope.Event, json.RawMessage, string) {
	fail := func(reason string) (envelope.Event, json.RawMessage, string) { return envelope.Event{}, nil, reason }
	if !a.validRun(room, act) {
		return fail(rejectBadAction)
	}
	if a.Requester == nil {
		return fail(rejectNoFactory)
	}
	// Before the request: a run lives outside the log, so a replay must never ask twice.
	switch prev, dup, err := a.Log.Stored(ctx, d); {
	case err != nil:
		return fail(rejectLogUnavailable)
	case dup && !isRunRequest(prev):
		return fail(rejectBadAction)
	case dup:
		return prev, nil, ""
	}
	switch busy, err := a.busy(ctx, room.Name); {
	case err != nil:
		return fail(rejectLogUnavailable)
	case busy:
		return fail(rejectRoomBusy)
	}
	evs, err := a.Log.BriefSources(ctx, room.Name) // the latest agent handoff and verdict only (review 4.4 M3, I3)
	if err != nil {
		return fail(rejectLogUnavailable)
	}
	forked, err := a.forkPoint(ctx, room)
	if err != nil {
		return fail(rejectLogUnavailable)
	}
	evs = append(evs, forked...)
	queued, err := a.Log.Queue(ctx, room.Name)
	if err != nil {
		return fail(rejectLogUnavailable)
	}
	req := runrequest.Request{Role: act.Role, Repository: room.Spec.Repository, BaseRef: brief.LastCommit(evs),
		Branch: "agent/" + room.Name, DataClass: room.Spec.DataClass, RoomRef: room.Name, Principal: p.ID,
		AccessToken: p.AccessToken, EgressProfiles: act.EgressProfiles,
		// The act's own key (review 4.4 I1): a retry after a failed record asks
		// for the same run, which the factory answers with the same runId.
		IdempotencyKey: d.RoomID + ":" + d.OriginClient + ":" + strconv.FormatInt(d.OriginSeq, 10)}
	if req.BaseRef == "" {
		req.BaseRef = "main"
	}
	var refs []int64
	if act.Role == "reviewer" { // ruling P24: its task is the PR, and the queue waits for a brief
		if req.TaskURL = act.PRURL; req.TaskURL == "" {
			req.TaskURL = brief.LastPR(evs, a.prs.For(room.Spec.Repository))
		}
		if req.TaskURL == "" {
			return fail(rejectNeedsPR)
		}
	} else {
		var quoted int
		req.TaskText, quoted = brief.Build(room.Name, act.Role, evs, queued, runrequest.NewID())
		for _, q := range queued[:quoted] {
			refs = append(refs, q.Ref)
		}
	}
	res, err := a.Requester.Request(ctx, req)
	switch {
	case errors.Is(err, runrequest.ErrBudget):
		return fail(rejectOverBudget)
	case errors.Is(err, runrequest.ErrForbidden):
		return fail(rejectNotPermitted)
	case err != nil || !envelope.ValidID(res.RunID):
		return fail(rejectNoFactory)
	}
	fields := map[string]any{"role": act.Role, "via": res.Via, "baseRef": req.BaseRef}
	if req.TaskURL != "" {
		fields["taskUrl"] = req.TaskURL // which PR the reviewer got (review 4.4 I3), checked by IsPR
	}
	ev, err := a.Log.RecordRunRequest(ctx, d, res.RunID, refs, fields)
	if err != nil {
		return fail(reason(err))
	}
	return ev, res.Manifest, ""
}

// forkPoint is a forked room's state_changed{forked_from}, the event after the
// prefix its annotation names, for the brief's Forked-from line; nothing for
// any other room. One read by seq, never a scan of the log.
func (a *Actor) forkPoint(ctx context.Context, room *v1alpha1.Room) ([]envelope.Event, error) {
	seq := roomctrl.ForkSeq(room)
	if seq == 0 {
		return nil, nil
	}
	return a.Log.Range(ctx, room.Name, seq, 1)
}
