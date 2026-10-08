// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/time/rate"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/bridge"
	"github.com/Smana/agent-platform/internal/brief"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/policy"
	"github.com/Smana/agent-platform/internal/roomctrl"
	"github.com/Smana/agent-platform/internal/runrequest"
	"github.com/Smana/agent-platform/internal/runwatch"
	"github.com/Smana/agent-platform/internal/store"
	"github.com/Smana/agent-platform/internal/wire"
)

// ActLog is the part of the store the actions write; *store.Store implements it.
type ActLog interface {
	Room(ctx context.Context, id string) (store.RoomState, error)
	Append(ctx context.Context, d envelope.Draft) (envelope.Event, bool, error)
	AppendAsDriver(ctx context.Context, driver string, epoch int64, d envelope.Draft) (envelope.Event, bool, error)
	Enqueue(ctx context.Context, d envelope.Draft, author, text string) (envelope.Event, error)
	PromoteQueued(ctx context.Context, roomID string, ref int64, driver string, epoch int64, runID string, d envelope.Draft) (envelope.Event, error)
	RemoveQueued(ctx context.Context, roomID string, ref int64, d envelope.Draft) (envelope.Event, error)
	ChangeDriver(ctx context.Context, roomID string, expect int64, to, reason string, d envelope.Draft) (envelope.Event, bool, error)
	DriverSeen(ctx context.Context, roomID, principal string, acted bool) error
	CloseRoom(ctx context.Context, roomID, reason string) error
	// start_run checks for a pending run, reads the brief's sources, then records
	// the run with what it consumed.
	PendingRuns(ctx context.Context, roomID string, within time.Duration) ([]string, error)
	BriefSources(ctx context.Context, roomID string) ([]envelope.Event, error)
	Queue(ctx context.Context, roomID string) ([]store.Queued, error)
	Stored(ctx context.Context, d envelope.Draft) (envelope.Event, bool, error)
	RecordRunRequest(ctx context.Context, d envelope.Draft, runID string, refs []int64, fields map[string]any) (envelope.Event, error)
	// decide reads the approval, then records the first valid decision (phase 5).
	Approval(ctx context.Context, id string) (store.Approval, error)
	Decide(ctx context.Context, approvalID, decision, by, reason string, d envelope.Draft) (envelope.Event, store.Approval, error)
	// fork reads the commit at its seq, then copies the prefix (phase 6); a
	// forked room's start_run reads its forked_from for the brief.
	BriefSourcesThrough(ctx context.Context, roomID string, seq int64) ([]envelope.Event, error)
	Fork(ctx context.Context, src string, upTo int64, r store.NewRoom, d envelope.Draft, fields map[string]any) (envelope.Event, error)
	Range(ctx context.Context, roomID string, afterSeq int64, limit int) ([]envelope.Event, error)
}

// Redactor removes secrets from a JSON payload before it is appended (§4);
// *redact.Redactor implements it.
type Redactor interface {
	Payload(ctx context.Context, raw json.RawMessage) (json.RawMessage, []string, error)
}

// Action is an act frame's action (docs/api.md, Actions).
type Action struct {
	Kind           string   `json:"kind"`
	Text           string   `json:"text,omitempty"`
	Delivery       string   `json:"delivery,omitempty"`
	Ref            int64    `json:"ref,omitempty"`
	To             string   `json:"to,omitempty"`
	Reason         string   `json:"reason,omitempty"`
	Role           string   `json:"role,omitempty"`
	PRURL          string   `json:"prUrl,omitempty"`
	EgressProfiles []string `json:"egressProfiles,omitempty"`
	Principal      string   `json:"principal,omitempty"`  // invite
	MemberRole     string   `json:"memberRole,omitempty"` // invite
	Approver       bool     `json:"approver,omitempty"`   // invite
	ApprovalID     string   `json:"approvalId,omitempty"` // decide
	Decision       string   `json:"decision,omitempty"`   // decide: approved | denied
	Seq            int64    `json:"seq,omitempty"`        // fork: the last event copied
	Note           string   `json:"note,omitempty"`       // fork
}

// Actor serves humans' act frames: every write a human makes to a room. Log,
// Groups, Runs and Redactor are required; Rooms is required for invite and new
// rooms, Requester for start_run.
type Actor struct {
	Log       ActLog
	Groups    policy.Groups
	Runs      Runs
	Redactor  Redactor
	Rooms     client.Client
	Requester runrequest.Requester
	OnReject  func(ctx context.Context, reason string)
	// OnDecided observes a human decision's wait since its request
	// (rooms_approval_decision_seconds).
	OnDecided func(time.Duration)

	mu       sync.Mutex
	limiters map[string]*rate.Limiter
	forks    map[string]*rate.Limiter
}

// The rejections an ack carries (docs/api.md).
const (
	rejectNotPermitted   = wire.ReasonNotPermitted
	rejectStaleEpoch     = "stale_epoch"
	rejectRateLimited    = wire.ReasonRateLimited
	rejectNoRunningRun   = "no_running_run"
	rejectBadAction      = "bad_action"
	rejectNotQueued      = "not_queued"
	rejectSealed         = wire.ReasonSealed
	rejectConflict       = "conflict" // the Room changed under an invite: retry
	rejectLogUnavailable = wire.ReasonLogUnavailable
	rejectRoomBusy       = wire.ReasonRoomBusy
	rejectOverBudget     = "over_budget"
	rejectNeedsPR        = "reviewer_needs_pr"
	rejectNoFactory      = "factory_unavailable"
	rejectDecided        = "already_decided" // another approver decided first, or it expired or was superseded
	rejectFourEyes       = "four_eyes"       // OD-16: the decider prompted the run
	rejectTooLarge       = "too_large"       // a fork's prefix over the store's caps
)

// maxReason bounds a take's reason, which the driver event carries.
const maxReason = 256

// maxDecisionReason bounds an approver's reason, which the bridge shows the agent
// cut to the same 1 KiB.
const maxDecisionReason = 1 << 10

// memberPrincipal is the Room CRD's pattern for a member.
var memberPrincipal = regexp.MustCompile(`^human:[A-Za-z0-9@._-]{1,255}$`)

// limited takes one of principal's actions: 10/s, burst 20 (§4); per replica (P22).
func (a *Actor) limited(principal string) bool {
	return !a.allow(&a.limiters, principal, 10, 20)
}

// forkLimited takes one of principal's forks, on top of limited: each copies a
// prefix in one transaction and keeps it under a fresh retention.
func (a *Actor) forkLimited(principal string) bool {
	return !a.allow(&a.forks, principal, rate.Every(time.Minute), 3)
}

func (a *Actor) allow(limiters *map[string]*rate.Limiter, principal string, r rate.Limit, burst int) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if *limiters == nil {
		*limiters = map[string]*rate.Limiter{}
	}
	l, ok := (*limiters)[principal]
	if !ok {
		l = rate.NewLimiter(r, burst)
		(*limiters)[principal] = l
	}
	return l.Allow()
}

var kinds = map[string]policy.Action{"remove_queued": policy.RemoveQueued, "promote_queued": policy.PromoteQueued,
	"interrupt": policy.Interrupt, "driver_request": policy.DriverRequest, "driver_give": policy.DriverGive,
	"driver_take": policy.DriverTake, "start_run": policy.StartRun, "invite": policy.Invite, "close": policy.Close,
	"decide": policy.Decide, "fork": policy.Fork}

var deliveries = map[string]policy.Action{"none": policy.Chat, "queued": policy.Queue, "steering": policy.Steer}

// fenced actions carry the driverEpoch they were decided on (§2); the store checks
// it again under the room's row lock.
var fenced = map[string]bool{"steering": true, "promote_queued": true, "interrupt": true, "driver_give": true}

// Handle serves one act frame of p in room, through the web UI or not (ruling
// P18), on the connection session, and returns its ack.
func (a *Actor) Handle(ctx context.Context, p authn.Principal, webUI bool, session string, room *v1alpha1.Room, f wire.ClientFrame) wire.ServerFrame {
	ack := wire.ServerFrame{Type: wire.FrameAck, ClientSeq: f.ClientSeq}
	reject := func(reason string) wire.ServerFrame {
		if a.OnReject != nil {
			a.OnReject(ctx, reason)
		}
		ack.Rejected = reason
		return ack
	}
	if a.limited(p.ID) {
		return reject(rejectRateLimited)
	}
	var act Action
	if json.Unmarshal(f.Action, &act) != nil || f.ClientSeq <= 0 {
		return reject(rejectBadAction)
	}
	if a.Redactor == nil {
		return reject(rejectLogUnavailable) // nothing is stored unredacted
	}
	st, err := a.Log.Room(ctx, room.Name)
	if err != nil {
		return reject(rejectLogUnavailable)
	}
	pa, ok := kinds[act.Kind]
	fenceKey := act.Kind
	if act.Kind == "message" {
		pa, ok = deliveries[act.Delivery]
		fenceKey = act.Delivery
	}
	if !ok {
		return reject(rejectBadAction)
	}
	sub := a.Groups.Resolve(room, p, st.Driver, webUI)
	if !policy.Allowed(sub, pa) {
		return reject(rejectNotPermitted)
	}
	// Every other action writes this room, and an invite must not change the Room
	// first (review 4.2 M2). A fork writes only the new room, but a sealed room
	// forks only for its owners, agents-admin included (ruling M3).
	if st.Sealed && (act.Kind != "fork" || sub.Role < policy.Owner) {
		return reject(rejectSealed)
	}
	if fenced[fenceKey] && (f.DriverEpoch == nil || *f.DriverEpoch != st.DriverEpoch) {
		return reject(rejectStaleEpoch)
	}
	d := envelope.Draft{RoomID: room.Name, Actor: envelope.Actor{Kind: envelope.ActorHuman, ID: p.ID},
		Origin: envelope.OriginClient, OriginClient: p.ID + ":" + session, OriginSeq: f.ClientSeq}
	var ev envelope.Event
	var why string
	switch act.Kind {
	case "start_run":
		ev, ack.Result, why = a.startRun(ctx, p, room, act, d)
	case "fork":
		ev, ack.Result, why = a.fork(ctx, p, sub.Role == policy.Owner, room, act, d)
	default:
		ev, why = a.dispatch(ctx, p, room, st, act, d)
	}
	if why != "" {
		return reject(why)
	}
	if sub.Driver {
		_ = a.Log.DriverSeen(ctx, room.Name, p.ID, true) // a no-op once the holder gave the token away
	}
	ack.Seq = ev.Seq
	return ack
}

func (a *Actor) running(room string) (runwatch.Run, bool) {
	for _, r := range a.Runs.InRoom(room) {
		if r.Live() {
			return r, true
		}
	}
	return runwatch.Run{}, false
}

// reason maps a store error to the ack's rejection.
func reason(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, store.ErrStaleEpoch):
		return rejectStaleEpoch
	case errors.Is(err, store.ErrNotQueued):
		return rejectNotQueued
	case errors.Is(err, store.ErrNotAuthor):
		return rejectNotPermitted
	case errors.Is(err, store.ErrSealed):
		return rejectSealed
	case errors.Is(err, store.ErrAlreadyDecided):
		return rejectDecided
	case errors.Is(err, store.ErrInvalidDriver), errors.Is(err, store.ErrKeyConflict), errors.Is(err, store.ErrNoApproval),
		errors.Is(err, store.ErrBadDecision), store.IsDataError(err):
		return rejectBadAction
	}
	return rejectLogUnavailable
}

func done(ev envelope.Event, err error) (envelope.Event, string) { return ev, reason(err) }

func done3(ev envelope.Event, _ bool, err error) (envelope.Event, string) { return ev, reason(err) }

// redacted runs d's payload through the redactor, before any append (§4).
func (a *Actor) redacted(ctx context.Context, d envelope.Draft) (envelope.Draft, error) {
	payload, rules, err := a.Redactor.Payload(ctx, d.Payload)
	if err != nil {
		return d, err
	}
	d.Payload, d.Redactions = payload, rules
	return d, nil
}

// redactText is the redactor over one string, such as a take's reason.
func (a *Actor) redactText(ctx context.Context, s string) (string, []string, error) {
	raw, rules, err := a.Redactor.Payload(ctx, envelope.Must(s))
	if err != nil {
		return "", nil, err
	}
	var out string
	return out, rules, json.Unmarshal(raw, &out)
}

func (a *Actor) dispatch(ctx context.Context, p authn.Principal, room *v1alpha1.Room, st store.RoomState, act Action, d envelope.Draft) (envelope.Event, string) {
	state := func(kind string, fields map[string]any) envelope.Draft {
		d.Type, d.Payload = envelope.StateChanged, envelope.StatePayload(kind, fields)
		return d
	}
	switch act.Kind {
	case "message":
		return a.message(ctx, room, st, act, d)
	case "remove_queued":
		return done(a.Log.RemoveQueued(ctx, room.Name, act.Ref, d)) // the author or the driver (§2)
	case "promote_queued":
		run, ok := a.running(room.Name)
		if !ok {
			return envelope.Event{}, rejectNoRunningRun
		}
		return done(a.Log.PromoteQueued(ctx, room.Name, act.Ref, p.ID, st.DriverEpoch, run.ID, d))
	case "interrupt":
		run, ok := a.running(room.Name)
		if !ok {
			return envelope.Event{}, rejectNoRunningRun
		}
		return done3(a.Log.AppendAsDriver(ctx, p.ID, st.DriverEpoch, state("interrupt", map[string]any{"runId": run.ID})))
	case "driver_request":
		if strings.HasPrefix(st.Driver, "system:") { // a system holder yields at once (§2)
			return done3(a.Log.ChangeDriver(ctx, room.Name, st.DriverEpoch, p.ID, "requested", d))
		}
		return done3(a.Log.Append(ctx, state("driver_request", map[string]any{"by": p.ID, "holder": st.Driver})))
	case "driver_give":
		if !a.receives(room, st, act.To) {
			return envelope.Event{}, rejectBadAction
		}
		return done3(a.Log.ChangeDriver(ctx, room.Name, st.DriverEpoch, act.To, "given", d))
	case "driver_take":
		why := strings.TrimSpace(act.Reason)
		if why == "" || len(why) > maxReason || !utf8.ValidString(why) {
			return envelope.Event{}, rejectBadAction // take needs a reason (§2), and a short one
		}
		why, rules, err := a.redactText(ctx, why)
		if err != nil {
			return envelope.Event{}, rejectLogUnavailable
		}
		d.Redactions = rules
		return done3(a.Log.ChangeDriver(ctx, room.Name, st.DriverEpoch, p.ID, "taken: "+why, d))
	case "invite":
		return a.invite(ctx, room, st, act, d)
	case "close":
		return envelope.Event{}, reason(a.Log.CloseRoom(ctx, room.Name, "closed by "+p.ID))
	case "decide":
		return a.decide(ctx, p, room, act, d)
	}
	return envelope.Event{}, rejectBadAction
}

// message appends a chat, queues a message, or steers the running run (§2).
func (a *Actor) message(ctx context.Context, room *v1alpha1.Room, st store.RoomState, act Action, d envelope.Draft) (envelope.Event, string) {
	if strings.TrimSpace(act.Text) == "" || len(act.Text) > envelope.MaxHumanMessage {
		return envelope.Event{}, rejectBadAction
	}
	payload := envelope.MessagePayload{Kind: envelope.KindChat, Text: act.Text, Delivery: envelope.Delivery(act.Delivery)}
	var run runwatch.Run
	if act.Delivery == "steering" {
		var ok bool
		if run, ok = a.running(room.Name); !ok {
			return envelope.Event{}, rejectNoRunningRun
		}
		payload.To = []string{"agent:" + run.ID}
	}
	d.Type, d.Payload = envelope.Message, envelope.Must(payload)
	d, err := a.redacted(ctx, d)
	if err != nil {
		return envelope.Event{}, rejectLogUnavailable
	}
	switch act.Delivery {
	case "queued":
		// The row keeps the redacted text: the next run's brief quotes it (review M7).
		var stored envelope.MessagePayload
		if err := json.Unmarshal(d.Payload, &stored); err != nil {
			return envelope.Event{}, rejectLogUnavailable
		}
		return done(a.Log.Enqueue(ctx, d, d.Actor.ID, stored.Text))
	case "steering":
		return done3(a.Log.AppendAsDriver(ctx, d.Actor.ID, st.DriverEpoch, d))
	}
	return done3(a.Log.Append(ctx, d))
}

// decide records an approver's decision on one of the room's approvals (§6).
// The store's UPDATE … WHERE pending, under the room's row lock, is the fence:
// the first valid decision wins and a later one gets already_decided. With
// four-eyes on, nobody who prompted the run decides (OD-16).
func (a *Actor) decide(ctx context.Context, p authn.Principal, room *v1alpha1.Room, act Action, d envelope.Draft) (envelope.Event, string) {
	why := strings.TrimSpace(act.Reason)
	if (act.Decision != store.ApprovalApproved && act.Decision != store.ApprovalDenied) || act.ApprovalID == "" ||
		len(why) > maxDecisionReason { // encoding/json already made it valid UTF-8
		return envelope.Event{}, rejectBadAction
	}
	ap, err := a.Log.Approval(ctx, act.ApprovalID)
	switch {
	case errors.Is(err, store.ErrNoApproval):
		return envelope.Event{}, rejectBadAction
	case err != nil:
		return envelope.Event{}, rejectLogUnavailable
	case ap.RoomID != room.Name: // another room's approval is no approval here
		return envelope.Event{}, rejectBadAction
	}
	if room.Spec.Approvals.FourEyes && slices.Contains(ap.Prompters, p.ID) {
		return envelope.Event{}, rejectFourEyes
	}
	if why != "" { // the agent reads it: redacted like every human payload (§4)
		if why, d.Redactions, err = a.redactText(ctx, why); err != nil {
			return envelope.Event{}, rejectLogUnavailable
		}
	}
	ev, _, err := a.Log.Decide(ctx, ap.ID, act.Decision, p.ID, why, d)
	if err == nil && ap.State == store.ApprovalPending && a.OnDecided != nil { // a replay was observed once already
		a.OnDecided(time.Since(ap.RequestedAt))
	}
	return done(ev, err)
}

// receives reports whether to may take the token from a give: a collaborator or
// better, or the room's own system holder, the one it falls back to.
func (a *Actor) receives(room *v1alpha1.Room, st store.RoomState, to string) bool {
	if strings.HasPrefix(to, "system:") {
		return to == room.Spec.Driver || to == st.FallbackDriver
	}
	sub := a.Groups.Resolve(room, authn.Principal{Kind: envelope.ActorHuman, ID: to}, "", true)
	return strings.HasPrefix(to, "human:") && sub.Role >= policy.Collaborator
}

// invite adds or changes a member on the Room CR and records it (§1: owner only).
func (a *Actor) invite(ctx context.Context, room *v1alpha1.Room, st store.RoomState, act Action, d envelope.Draft) (envelope.Event, string) {
	if !memberPrincipal.MatchString(act.Principal) || policy.ParseRole(act.MemberRole) == policy.None {
		return envelope.Event{}, rejectBadAction
	}
	if a.Rooms == nil {
		return envelope.Event{}, rejectLogUnavailable
	}
	updated := room.DeepCopy()
	members := []v1alpha1.Member{}
	for _, m := range updated.Spec.Members {
		if m.Principal != act.Principal {
			members = append(members, m)
		}
	}
	if len(members) >= 20 { // the CRD's MaxItems
		return envelope.Event{}, rejectBadAction
	}
	members = append(members, v1alpha1.Member{Principal: act.Principal, Role: act.MemberRole, Approver: act.Approver})
	updated.Spec.Members = members
	// The token stays with a collaborator or better: the holder hands it over before
	// being demoted (review 4.2 M6).
	if act.Principal == st.Driver &&
		a.Groups.Resolve(updated, authn.Principal{Kind: envelope.ActorHuman, ID: st.Driver}, "", true).Role < policy.Collaborator {
		return envelope.Event{}, rejectBadAction
	}
	// Update, not a patch: the room's resourceVersion makes a concurrent change a conflict.
	if err := a.Rooms.Update(ctx, updated); err != nil {
		switch {
		case apierrors.IsConflict(err):
			return envelope.Event{}, rejectConflict
		case apierrors.IsInvalid(err):
			return envelope.Event{}, rejectBadAction
		}
		return envelope.Event{}, rejectLogUnavailable
	}
	d.Type = envelope.Participant
	d.Payload = envelope.Must(envelope.ParticipantPayload{Principal: act.Principal, Change: "role_changed",
		Role: act.MemberRole, Approver: act.Approver})
	return done3(a.Log.Append(ctx, d))
}

// runRoles are the roles a human starts (§1); egressProfile a profile's name.
var (
	runRoles      = map[string]bool{"implementer": true, "reviewer": true, "tester": true, "triager": true}
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
// name, and a PR only for a reviewer and only of the room's repository.
func validRun(room *v1alpha1.Room, act Action) bool {
	if !runRoles[act.Role] || len(act.EgressProfiles) > maxEgressProfiles ||
		(act.PRURL != "" && (act.Role != "reviewer" || !brief.IsPR(act.PRURL, room.Spec.Repository))) {
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
	if !validRun(room, act) {
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
			req.TaskURL = brief.LastPR(evs, room.Spec.Repository)
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

// ForkedFrom annotates a forked Room with <source room>@<seq>; roomctrl owns it, since the
// Room's task status must skip the copied prefix.
const ForkedFrom = roomctrl.ForkedFrom

// maxNote bounds a fork's note, which its forked_from event carries.
const maxNote = 1 << 10

// fork branches the room at act.Seq into a new room the forker owns and drives
// (§5): the log prefix with its seqs, then state_changed{forked_from} with the
// commit at that seq, then, for a role, the new room's first run, on its own
// branch and the forker's token. Nothing is written to the source room, so a
// sealed one forks too, for its owners. The source's approvals carry over for
// its owners; anyone else gets, class by class, the stricter of them and a new
// room's (ruling M2). The ack's result is {roomId, run?, runError?}: a run that
// fails leaves the fork made.
func (a *Actor) fork(ctx context.Context, p authn.Principal, owner bool, room *v1alpha1.Room, act Action, d envelope.Draft) (envelope.Event, json.RawMessage, string) {
	fail := func(reason string) (envelope.Event, json.RawMessage, string) { return envelope.Event{}, nil, reason }
	note := strings.TrimSpace(act.Note)
	if act.Seq < 1 || len(note) > maxNote || (act.Role != "" && !validRun(room, act)) || !memberPrincipal.MatchString(p.ID) {
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
	if !owner {
		// A new room's policy is the CRD's default: createRoom sets none.
		s := bridge.Stricter(wire.ApprovalPolicy{Profile: approvals.Profile, Overrides: approvals.Overrides}, wire.ApprovalPolicy{})
		approvals.Profile, approvals.Overrides = s.Profile, s.Overrides
	}
	child := &v1alpha1.Room{ObjectMeta: metav1.ObjectMeta{Name: id, Namespace: room.Namespace,
		Annotations: map[string]string{ForkedFrom: fmt.Sprintf("%s@%d", room.Name, act.Seq)}},
		Spec: v1alpha1.RoomSpec{Owner: p.ID, Driver: p.ID, DataClass: room.Spec.DataClass, Repository: room.Spec.Repository,
			Approvals: approvals, Retention: room.Spec.Retention}}
	if err := a.Rooms.Create(ctx, child); err != nil {
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
