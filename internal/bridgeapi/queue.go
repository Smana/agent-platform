// SPDX-License-Identifier: Apache-2.0

package bridgeapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/store"
	"github.com/Smana/agent-platform/internal/wire"
)

// QueueStore is the room queue (SP2 phase 4) as system callers reach it: SP3's
// factory queues a maintainer's GitHub review in the task's room, and consumes
// what its next run's brief quoted (SP3 R9). *store.Store implements it.
type QueueStore interface {
	Enqueue(ctx context.Context, d envelope.Draft, author, text string) (envelope.Event, error)
	Queue(ctx context.Context, roomID string) ([]store.Queued, error)
	SetQueued(ctx context.Context, roomID string, ref int64, to, runID string) error
	Cursor(ctx context.Context, roomID, originClient string) (int64, error)
}

const (
	// maxConsume bounds one consume's refs: each is a database round trip, inside routeTimeout.
	maxConsume      = 100
	maxConsumeBytes = 8 << 10
)

// A stream names one source of queued messages (GitHub reviews, CI failures),
// each with its own clientSeq space. No colon: it ends the origin.
var streamRE = regexp.MustCompile(`^[a-z]{1,16}$`)

// queueRoute admits a system caller to a room's queue: authenticated first, then
// a wired queue store (501 otherwise), the caller's limits and a C2 room id.
func (s *Server) queueRoute(h func(w http.ResponseWriter, r *http.Request, principal, room string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.systemAuth(w, r)
		if !ok {
			return
		}
		if s.Queue == nil {
			fail(w, http.StatusNotImplemented, wire.ReasonNoQueue)
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
		h(w, r, p.ID, id)
	}
}

// enqueue: POST /v1/rooms/{id}/queue {text, clientSeq, stream}. The origin is
// <principal>:queue:<stream>, apart from the principal's task_state messages. The
// stream's latest clientSeq replayed answers 200, nothing stored. A lower one goes
// to the store, which dedupes on the exact key: GitHub review ids, Task 2.3's
// clientSeq, follow creation order, not submission order.
func (s *Server) enqueue(w http.ResponseWriter, r *http.Request, principal, room string) {
	var in struct {
		Text      string `json:"text"`
		ClientSeq int64  `json:"clientSeq"`
		Stream    string `json:"stream"`
	}
	if err := decodeStrict(r.Body, &in); err != nil ||
		in.ClientSeq <= 0 || strings.TrimSpace(in.Text) == "" || len(in.Text) > envelope.MaxHumanMessage {
		fail(w, http.StatusBadRequest, wire.ReasonBadMessage)
		return
	}
	if in.Stream == "" {
		in.Stream = "default"
	}
	if !streamRE.MatchString(in.Stream) {
		fail(w, http.StatusBadRequest, wire.ReasonBadStream)
		return
	}
	origin := principal + ":queue:" + in.Stream
	cur, err := s.Queue.Cursor(r.Context(), room, origin)
	if err != nil {
		s.logFailure(w, err, "read a queue stream's cursor", "room", room, "principal", principal)
		return
	}
	if in.ClientSeq == cur {
		reply(w, http.StatusOK, map[string]bool{"duplicate": true})
		return
	}
	// Payload, not String: it also strips NUL, which Postgres text refuses, and
	// honours the request's deadline. The store does not redact.
	payload, rules, err := s.Redactor.Payload(r.Context(), envelope.Must(envelope.MessagePayload{
		Kind: envelope.KindChat, Text: in.Text, Delivery: envelope.DeliveryQueued}))
	if err != nil {
		s.log().Warn("queued message not redacted in time", "room", room, "principal", principal, "err", err)
		fail(w, http.StatusServiceUnavailable, wire.ReasonTimedOut)
		return
	}
	// The row keeps the redacted text, which must equal the event's (trigger queue_is_its_event).
	var stored envelope.MessagePayload
	if err := json.Unmarshal(payload, &stored); err != nil {
		s.logFailure(w, err, "read a redacted queued message", "room", room)
		return
	}
	ev, err := s.Queue.Enqueue(r.Context(), envelope.Draft{RoomID: room,
		Actor: envelope.Actor{Kind: envelope.ActorSystem, ID: principal}, Type: envelope.Message,
		Origin: envelope.OriginClient, OriginClient: origin, OriginSeq: in.ClientSeq,
		Redactions: rules, Payload: payload}, principal, stored.Text)
	if err != nil {
		s.logFailure(w, err, "queue a system message", "room", room, "principal", principal)
		return
	}
	reply(w, http.StatusCreated, map[string]int64{"seq": ev.Seq})
}

// listQueue: GET /v1/rooms/{id}/queue, the room's messages still queued, oldest first.
func (s *Server) listQueue(w http.ResponseWriter, r *http.Request, _, room string) {
	q, err := s.Queue.Queue(r.Context(), room)
	if err != nil {
		s.logFailure(w, err, "read a room's queue", "room", room)
		return
	}
	type item struct {
		Ref    int64  `json:"ref"`
		Author string `json:"author"`
		Text   string `json:"text"`
	}
	out := make([]item, 0, len(q))
	for _, x := range q {
		out = append(out, item{Ref: x.Ref, Author: x.Author, Text: x.Text})
	}
	reply(w, http.StatusOK, map[string]any{"queued": out})
}

// consume: POST /v1/rooms/{id}/queue/consume {refs, runId}, the messages runId's
// brief quoted. One no longer queued (consumed, promoted, removed) or never
// queued is skipped, so a retry is safe; consumed counts this call's moves.
func (s *Server) consume(w http.ResponseWriter, r *http.Request, _, room string) {
	var in struct {
		Refs  []int64 `json:"refs"`
		RunID string  `json:"runId"`
	}
	if err := decodeStrict(r.Body, &in); err != nil || !envelope.ValidID(in.RunID) || len(in.Refs) > maxConsume {
		fail(w, http.StatusBadRequest, wire.ReasonBadConsume)
		return
	}
	n := 0
	for _, ref := range in.Refs {
		switch err := s.Queue.SetQueued(r.Context(), room, ref, "consumed", in.RunID); {
		case err == nil:
			n++
		case errors.Is(err, store.ErrNotQueued):
		default:
			s.logFailure(w, err, "consume a queued message", "room", room, "ref", ref)
			return
		}
	}
	reply(w, http.StatusOK, map[string]int{"consumed": n})
}
