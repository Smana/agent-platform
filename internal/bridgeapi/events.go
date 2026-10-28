// SPDX-License-Identifier: Apache-2.0

package bridgeapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/Smana/maku/internal/authn"
	"github.com/Smana/maku/internal/envelope"
	"github.com/Smana/maku/internal/redact"
	"github.com/Smana/maku/internal/runwatch"
	"github.com/Smana/maku/internal/store"
	"github.com/Smana/maku/internal/wire"
)

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	p, run, ok := s.bridgeAuth(w, r)
	if !ok {
		return
	}
	release, ok := s.admit(w, p.ID)
	if !ok {
		return
	}
	defer release()
	ctx := r.Context()
	var b wire.Batch
	if err := decodeStrict(r.Body, &b); err != nil {
		if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
			fail(w, http.StatusRequestEntityTooLarge, wire.ReasonBatchTooLarge)
			return
		}
		fail(w, http.StatusBadRequest, wire.ReasonBadBatch)
		return
	}
	if len(b.Items) > maxBatchItems {
		fail(w, http.StatusRequestEntityTooLarge, wire.ReasonBatchTooLarge)
		return
	}
	// The whole batch is checked before anything is written: a refused item never
	// leaves half a batch in the log.
	drafts := make([]envelope.Draft, len(b.Items))
	for i, it := range b.Items {
		d, err := s.bridgeDraft(ctx, p, run, it)
		var refused refusal
		switch {
		case errors.As(err, &refused):
			// The type is logged only when valid: an invalid one is the sender's text.
			typ := "invalid"
			if it.Type.Valid() {
				typ = string(it.Type)
			}
			s.log().Warn("bridge item refused", "room", run.Room, "run", run.ID, "type", typ, "reason", string(refused))
			fail(w, http.StatusBadRequest, string(refused))
			return
		case err != nil:
			s.log().Warn("bridge batch not redacted in time", "room", run.Room, "run", run.ID, "err", err)
			fail(w, http.StatusServiceUnavailable, wire.ReasonTimedOut)
			return
		}
		drafts[i] = d
	}
	// The holder's pushes renew the room's bridge lease (P17). A bridge another run
	// displaced learns it here and must stop: 409, never a silent append (Ruling Y).
	held, err := s.Log.TouchBridge(ctx, run.Room, run.ID)
	if err != nil {
		s.logFailure(w, err, "renew the bridge lease", "room", run.Room, "run", run.ID)
		return
	}
	if !held {
		fail(w, http.StatusConflict, wire.ReasonLeaseLost)
		return
	}
	var ack wire.BatchAck
	for i, d := range drafts {
		_, _, err := s.Log.AppendAsBridge(ctx, run.ID, d)
		if store.IsDataError(err) {
			// A value PostgreSQL refuses can never be stored: keep the slot with a stub, as
			// for an oversize payload, so the bridge's cursor moves on (review I6).
			s.log().Error("payload refused by the database", "room", run.Room, "run", run.ID, "type", d.Type, errAttr(err))
			d.Payload, d.Redactions = refusedStub(d.Type, StubInvalidValue), nil
			_, _, err = s.Log.AppendAsBridge(ctx, run.ID, d)
		}
		if err != nil {
			s.logFailure(w, err, "append a bridge event", "room", run.Room, "run", run.ID, "type", d.Type)
			return
		}
		if it := b.Items[i]; it.Stream == wire.StreamEvents {
			ack.AfterHarnessSeq = max(ack.AfterHarnessSeq, it.Seq)
		} else {
			ack.AfterStatusSeq = max(ack.AfterStatusSeq, it.Seq)
		}
	}
	reply(w, http.StatusOK, ack)
}

// refusal is an item refused for what it is, carrying its wire reason.
type refusal string

func (r refusal) Error() string { return "bridge item refused: " + string(r) }

// bridgeDraft redacts and checks one pushed item. A refusal means the item may not
// be pushed; any other error, that ctx ended before the redaction finished.
func (s *Server) bridgeDraft(ctx context.Context, p authn.Principal, run runwatch.Run, it wire.Item) (envelope.Draft, error) {
	if it.Seq <= 0 || (it.Stream != wire.StreamEvents && it.Stream != wire.StreamStatus) || !it.Type.Valid() {
		return envelope.Draft{}, refusal(wire.ReasonBadItem)
	}
	d := envelope.Draft{RoomID: run.Room, RunID: run.ID,
		Actor: envelope.Actor{Kind: envelope.ActorAgent, ID: p.ID, Role: run.Role}, Type: it.Type,
		Origin: envelope.OriginHarness, OriginClient: originClient(run.ID, it.Stream), OriginSeq: it.Seq}
	redacted, rules, err := s.Redactor.Payload(ctx, it.Payload)
	switch {
	case errors.Is(err, redact.ErrKeyCollision):
		// Keys that are one once redacted: as often an env dump holding two
		// tokens as an attack. Neither value can be kept, so the item alone keeps
		// its slot as a stub, as for a value PostgreSQL refuses, and its batch
		// goes on (Ruling AI).
		s.log().Warn("bridge item stored as a stub: its keys collide once redacted", "room", run.Room, "run", run.ID, "type", it.Type)
		d.Payload = refusedStub(it.Type, StubKeyCollision)
		return d, nil
	case err != nil && ctx.Err() != nil:
		return envelope.Draft{}, err
	case err != nil:
		return envelope.Draft{}, refusal(wire.ReasonBadPayload)
	}
	payload, reason := bridgePayload(it.Type, redacted)
	if reason != "" {
		return envelope.Draft{}, refusal(reason)
	}
	d.Payload, d.Redactions = payload, rules
	return d, nil
}

// Why the broker stored an item as a stub rather than as sent. Each is the stub's
// reason field, and a label of rooms_bridge_items_stubbed_total (S1 review I-4).
const (
	// StubInvalidValue is a value the store rejected (SQLSTATE class 22, such as a NUL in jsonb).
	StubInvalidValue = "invalid_value"
	// StubKeyCollision is keys that are one once redacted: case-folded or NUL-equal (Ruling AI).
	StubKeyCollision = "key_collision"
)

// refusedStub stands in for a payload that cannot be stored: the slot is kept,
// so the bridge's cursor moves on (review I6), and it says why.
func refusedStub(t envelope.Type, why string) json.RawMessage {
	return envelope.Must(map[string]any{"refused": true, "type": t, "reason": why})
}
