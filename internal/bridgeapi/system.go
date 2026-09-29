// SPDX-License-Identifier: Apache-2.0

package bridgeapi

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/wire"
)

const (
	defaultRange = 100
	maxRange     = 500
)

// systemAuth admits an allowlisted system:* caller (ruling P3).
func (s *Server) systemAuth(w http.ResponseWriter, r *http.Request) (authn.Principal, bool) {
	p, err := s.Systems.Authenticate(r)
	switch {
	case errors.Is(err, authn.ErrForbidden):
		s.log().Info("system caller not allowlisted", "path", r.URL.Path)
		fail(w, http.StatusForbidden, wire.ReasonNotPermitted)
		return p, false
	case err != nil || p.ID == "":
		s.log().Debug("system credential refused", "path", r.URL.Path, "why", authWhy(err))
		fail(w, http.StatusUnauthorized, wire.ReasonUnauthenticated)
		return p, false
	}
	return p, true
}

// roomEvents: GET /v1/rooms/{id}/events?afterSeq=&limit= (SP3 reads verdicts here).
func (s *Server) roomEvents(w http.ResponseWriter, r *http.Request) {
	p, ok := s.systemAuth(w, r)
	if !ok {
		return
	}
	release, ok := s.admit(w, p.ID)
	if !ok {
		return
	}
	defer release()
	id := r.PathValue("id")
	if !envelope.ValidID(id) {
		fail(w, http.StatusBadRequest, wire.ReasonBadRoom)
		return
	}
	after, _ := strconv.ParseInt(r.URL.Query().Get("afterSeq"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > maxRange {
		limit = defaultRange
	}
	// Review M4: never a lastSeq of 0 because the database is down.
	st, err := s.Log.Room(r.Context(), id)
	if err != nil {
		s.logFailure(w, err, "read a room", "room", id)
		return
	}
	evs, err := s.Log.Range(r.Context(), id, max(after, 0), limit)
	if err != nil {
		s.logFailure(w, err, "read a room's events", "room", id)
		return
	}
	if evs == nil {
		evs = []envelope.Event{}
	}
	reply(w, http.StatusOK, map[string]any{"events": evs, "lastSeq": st.LastSeq})
}

// roomMessage: POST /v1/rooms/{id}/messages, system:* only, reserved kind task_state
// (C4). 201 for a new event, 200 for a replayed clientSeq; both carry its seq.
// A replay is keyed on (principal, clientSeq) alone: a different body under a
// seen clientSeq also answers 200 with the original seq, and is not stored.
func (s *Server) roomMessage(w http.ResponseWriter, r *http.Request) {
	p, ok := s.systemAuth(w, r)
	if !ok {
		return
	}
	release, ok := s.admit(w, p.ID)
	if !ok {
		return
	}
	defer release()
	id := r.PathValue("id")
	if !envelope.ValidID(id) { // review M4: 400, not a 503 from the draft check
		fail(w, http.StatusBadRequest, wire.ReasonBadRoom)
		return
	}
	var in struct {
		Kind      envelope.MessageKind `json:"kind"`
		Text      string               `json:"text"`
		ClientSeq int64                `json:"clientSeq"`
	}
	if err := decodeStrict(r.Body, &in); err != nil ||
		in.Kind != envelope.KindTaskState || in.ClientSeq <= 0 || len(in.Text) > envelope.MaxHumanMessage {
		fail(w, http.StatusBadRequest, wire.ReasonBadMessage)
		return
	}
	payload, rules, err := s.Redactor.Payload(r.Context(), envelope.Must(envelope.MessagePayload{
		Kind: in.Kind, Text: in.Text, Delivery: envelope.DeliveryNone}))
	if err != nil { // the struct always marshals: only an ended request fails here
		s.log().Warn("system message not redacted in time", "room", id, "principal", p.ID, "err", err)
		fail(w, http.StatusServiceUnavailable, wire.ReasonTimedOut)
		return
	}
	ev, dup, err := s.Log.Append(r.Context(), envelope.Draft{RoomID: id,
		Actor: envelope.Actor{Kind: envelope.ActorSystem, ID: p.ID}, Type: envelope.Message,
		Origin: envelope.OriginClient, OriginClient: p.ID, OriginSeq: in.ClientSeq,
		Redactions: rules, Payload: payload})
	if err != nil {
		s.logFailure(w, err, "append a system message", "room", id, "principal", p.ID)
		return
	}
	code := http.StatusOK
	if !dup {
		code = http.StatusCreated
		s.appended(ev)
	}
	reply(w, code, map[string]int64{"seq": ev.Seq})
}
