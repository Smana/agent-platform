// SPDX-License-Identifier: Apache-2.0

package bridgeapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/redact"
	"github.com/Smana/agent-platform/internal/store"
	"github.com/Smana/agent-platform/internal/wire"
)

// Approvals is the part of the store the approval route writes; *store.Store implements it.
type Approvals interface {
	RequestApproval(ctx context.Context, a store.Approval, d envelope.Draft) (store.Approval, envelope.Event, error)
	Prompters(ctx context.Context, roomID, runID string) ([]string, error)
}

const (
	// maxApprovalBytes caps a request (docs/api.md).
	maxApprovalBytes = 64 << 10
	// maxApprovalAction leaves the approval_requested payload's other fields room
	// under envelope.MaxPayload once the action is redacted.
	maxApprovalAction = envelope.MaxPayload - 1<<10
	// maxApprovalID bounds an eventId or a callId, which the row and its card keep.
	maxApprovalID = 256
	// The waits of §6: attended, and unattended when the Room's ttl is unreadable.
	attendedTTL   = 30 * time.Minute
	unattendedTTL = 4 * time.Hour
)

// approvalClasses are the classes the room's policy may send to approvers (§6):
// never plain, never egress.new, which no human can approve.
var approvalClasses = []string{"forge.push", "forge.pr", "forge.other", "mcp.write", "shell.high"}

// approvalTTL is when an approval expires: 30 minutes attended, the Room's ttl
// unattended (5.2 contract 2: always set).
func approvalTTL(p wire.ApprovalPolicy) time.Duration {
	if p.Profile != "unattended" {
		return attendedTTL
	}
	if ttl, err := time.ParseDuration(p.TTL); err == nil && ttl > 0 {
		return ttl
	}
	return unattendedTTL
}

// policy is the room's approval policy, attended when unknown.
func (s *Server) policy(roomID string) wire.ApprovalPolicy {
	if s.RoomPolicy == nil {
		return wire.ApprovalPolicy{Profile: "attended"}
	}
	return s.RoomPolicy(roomID)
}

func validApproval(in wire.ApprovalRequest) bool {
	return in.EventID != "" && len(in.EventID) <= maxApprovalID && in.CallID != "" && len(in.CallID) <= maxApprovalID &&
		slices.Contains(approvalClasses, in.Class)
}

// approval serves POST /v1/bridge/approvals: a pending action whose class the
// room's policy sends to its approvers (§6). The action is redacted and stored
// as the card shows it (T3). The prompters are read here, so a failed read
// refuses the request rather than weaken four-eyes.
func (s *Server) approval(w http.ResponseWriter, r *http.Request) {
	p, run, ok := s.bridgeAuth(w, r)
	if !ok {
		return
	}
	release, ok := s.admit(w, p.ID)
	if !ok {
		return
	}
	defer release()
	if s.Approvals == nil {
		fail(w, http.StatusServiceUnavailable, wire.ReasonLogUnavailable)
		return
	}
	ctx := r.Context()
	var in wire.ApprovalRequest
	if err := decodeStrict(r.Body, &in); err != nil || !validApproval(in) {
		fail(w, http.StatusBadRequest, wire.ReasonBadApproval)
		return
	}
	action, rules, err := s.Redactor.Payload(ctx, in.Action)
	switch {
	case err != nil && ctx.Err() != nil && !errors.Is(err, redact.ErrKeyCollision):
		fail(w, http.StatusServiceUnavailable, wire.ReasonTimedOut)
		return
	case err != nil, len(action) > maxApprovalAction, !isObject(action):
		fail(w, http.StatusBadRequest, wire.ReasonBadAction)
		return
	}
	prompters, err := s.Approvals.Prompters(ctx, run.Room, run.ID)
	if err != nil {
		s.logFailure(w, err, "read a run's prompters", "room", run.Room, "run", run.ID)
		return
	}
	if run.Principal != "" && !slices.Contains(prompters, run.Principal) {
		prompters = append(prompters, run.Principal)
	}
	a, _, err := s.Approvals.RequestApproval(ctx, store.Approval{ID: ulid.Make().String(), RoomID: run.Room, RunID: run.ID,
		EventID: in.EventID, CallID: in.CallID, Class: in.Class, Action: action, Prompters: prompters,
		ExpiresAt: time.Now().Add(approvalTTL(s.policy(run.Room)))},
		envelope.Draft{Actor: envelope.Actor{Kind: envelope.ActorAgent, ID: p.ID, Role: run.Role}, Origin: envelope.OriginHarness,
			OriginClient: "agent:" + run.ID + ":approvals", OriginSeq: time.Now().UnixNano(), Redactions: rules})
	if err != nil {
		s.logFailure(w, err, "request an approval", "room", run.Room, "run", run.ID, "class", in.Class)
		return
	}
	reply(w, http.StatusOK, wire.ApprovalAck{ApprovalID: a.ID, ExpiresAt: a.ExpiresAt.UTC()})
}

// isObject reports whether raw is a JSON object.
func isObject(raw json.RawMessage) bool {
	var obj map[string]json.RawMessage
	return json.Unmarshal(raw, &obj) == nil && obj != nil
}
