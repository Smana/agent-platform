// SPDX-License-Identifier: Apache-2.0

// Package bridgeapi serves :8443, over TLS only (GP-18): run bridges push their
// harness events here (C4: runs push, the broker never dials into a sandbox),
// and system principals read and annotate logs (SP3's API).
//
// Every refusal is a wire.Error whose reason is a wire.Reason* constant. The
// log's sentinel errors map to status codes in one place, logFailure.
package bridgeapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/runwatch"
	"github.com/Smana/agent-platform/internal/store"
	"github.com/Smana/agent-platform/internal/wire"
)

const (
	// A bridge seen within this window holds its room's lease (ruling P17, kept in
	// the store: review I7). It pushes every second, so two minutes of silence means gone.
	connectedWindow = 2 * time.Minute

	// Request bounds. A bridge sends at most 100 items per batch (Task 1.11).
	maxBatchBytes   = 2 << 20
	maxBatchItems   = 500
	maxMessageBytes = 2 * envelope.MaxHumanMessage
	// routeTimeout bounds each non-streaming route, reading, working and writing:
	// the listener's WriteTimeout is 0 for the stream's sake.
	routeTimeout = 30 * time.Second

	// The stream (§4): a 30 s ping, and a life of min(token exp, 1 h).
	defaultPing   = 30 * time.Second
	maxStreamLife = time.Hour
	// streamWriteWait bounds one write to a stream, so a bridge that stops
	// reading cannot pin a handler until its token expires.
	streamWriteWait = 10 * time.Second

	brokerActor = "system:room-broker"
)

// Authenticator maps a request's credential to a principal. *authn.Runs and
// *authn.Systems implement it; authn.ErrForbidden means a valid credential for a
// principal outside the allowlist.
type Authenticator interface {
	Authenticate(r *http.Request) (authn.Principal, error)
}

// Log is the part of the store the API reads and writes. A bridge's events go
// only through AppendAsBridge, fenced on the room's bridge lease (Ruling Y).
type Log interface {
	Append(ctx context.Context, d envelope.Draft) (envelope.Event, bool, error)
	AppendAsBridge(ctx context.Context, bridgeRun string, d envelope.Draft) (envelope.Event, bool, error)
	Range(ctx context.Context, roomID string, afterSeq int64, limit int) ([]envelope.Event, error)
	Cursor(ctx context.Context, roomID, originClient string) (int64, error)
	Room(ctx context.Context, id string) (store.RoomState, error)
	ClaimBridge(ctx context.Context, roomID, runID string, stale time.Duration, live func(ctx context.Context, runID string) bool) (string, bool, error)
	TouchBridge(ctx context.Context, roomID, runID string) (held bool, err error)
}

// Redactor removes secrets from a JSON payload; *redact.Redactor implements it.
type Redactor interface {
	Payload(raw json.RawMessage) (json.RawMessage, []string, error)
}

// Liveness answers whether a run is live; *runwatch.Watcher implements it.
type Liveness interface {
	Live(runID string) (runwatch.Run, bool)
}

// Server is the :8443 API. Log, Redactor, Runs, Systems and Watch are required;
// the rest is optional.
type Server struct {
	Log      Log
	Redactor Redactor
	Runs     Authenticator
	Systems  Authenticator
	Watch    Liveness
	// Notify, when set, is told of every new event: a fan-out hint (phase 2). It
	// runs on the request and must not block.
	Notify func(roomID string, seq int64)
	// RoomPolicy, when set, is the room's approval policy handed out at hello (phase 5).
	RoomPolicy func(roomID string) wire.ApprovalPolicy
	// OnAppend, when set, is told of every new event, for the metrics. It must not block.
	OnAppend func(ctx context.Context, t envelope.Type, o envelope.Origin, redactions []string)
	// PingEvery is the stream's keep-alive period; 0 means 30 s.
	PingEvery time.Duration
	// Ticker starts the stream's keep-alive; nil means a time.Ticker.
	Ticker func(d time.Duration) (c <-chan time.Time, stop func())
	// Now is the clock of request and stream deadlines; nil means time.Now.
	Now    func() time.Time
	Logger *slog.Logger
	conns  registry
}

// Routes is the :8443 handler.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/bridge/hello", s.bounded(0, s.hello))
	mux.HandleFunc("POST /v1/bridge/events", s.bounded(maxBatchBytes, s.events))
	mux.HandleFunc("GET /v1/bridge/stream", s.stream)
	mux.HandleFunc("GET /v1/rooms/{id}/events", s.bounded(0, s.roomEvents))
	mux.HandleFunc("POST /v1/rooms/{id}/messages", s.bounded(maxMessageBytes, s.roomMessage))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Every response, the mux's own 404 and 405 included, has a write deadline;
		// the stream replaces it with one per write.
		// ErrNotSupported only from a writer that is not a connection (a test's recorder).
		_ = http.NewResponseController(w).SetWriteDeadline(s.now().Add(routeTimeout))
		mux.ServeHTTP(w, r)
	})
}

// Drop ends a run's stream on this replica; register it with Watcher.OnGone (S4).
func (s *Server) Drop(_ context.Context, r runwatch.Run) { s.conns.drop(r.ID) }

// closeStreams ends every stream on this replica, at shutdown.
func (s *Server) closeStreams() { s.conns.dropAll() }

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Server) log() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.New(slog.DiscardHandler)
}

// bounded caps a non-streaming route's body and its work; Routes caps its response.
func (s *Server) bounded(maxBody int64, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithDeadline(r.Context(), s.now().Add(routeTimeout))
		defer cancel()
		r = r.WithContext(ctx)
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		h(w, r)
	}
}

func fail(w http.ResponseWriter, code int, reason string) {
	reply(w, code, wire.Error{Reason: reason})
}

func reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// logFailure answers a failed log read or write, mapping the store's sentinels
// to a status in one place, and logs what is not the caller's doing.
func (s *Server) logFailure(w http.ResponseWriter, err error, msg string, attrs ...any) {
	switch {
	case errors.Is(err, store.ErrLeaseLost):
		fail(w, http.StatusConflict, wire.ReasonLeaseLost)
	case errors.Is(err, store.ErrSealed):
		fail(w, http.StatusGone, wire.ReasonSealed)
	case errors.Is(err, store.ErrNoRoom):
		fail(w, http.StatusNotFound, wire.ReasonNoRoom)
	default:
		s.log().Error(msg, append(attrs, errAttr(err))...)
		fail(w, http.StatusServiceUnavailable, wire.ReasonLogUnavailable)
	}
}

// errAttr names a failure for the log. A database error's text can quote the
// value it refused, a payload, so only its SQLSTATE is logged.
func errAttr(err error) slog.Attr {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return slog.String("sqlstate", pg.Code)
	}
	return slog.String("err", err.Error())
}

// authWhy classifies a refused credential for the log; a token and its claims never reach it.
func authWhy(err error) string {
	for _, c := range []struct {
		err error
		why string
	}{
		{authn.ErrForbidden, "not_allowlisted"}, {authn.ErrTokenExpired, "expired"},
		{authn.ErrTokenNotYetValid, "not_yet_valid"}, {authn.ErrWrongIssuer, "wrong_issuer"},
		{authn.ErrWrongAudience, "wrong_audience"}, {authn.ErrUnknownKey, "unknown_key"},
		{authn.ErrKeysStale, "keys_stale"},
	} {
		if errors.Is(err, c.err) {
			return c.why
		}
	}
	return "invalid"
}

// appended tells the optional hooks of a new event.
func (s *Server) appended(ctx context.Context, ev envelope.Event) {
	if s.Notify != nil {
		s.Notify(ev.RoomID, ev.Seq)
	}
	if s.OnAppend != nil {
		s.OnAppend(ctx, ev.Type, ev.Origin, ev.Redactions)
	}
}

// bridgeAuth admits a run token for a run that is live and names a room (§1 Admission).
func (s *Server) bridgeAuth(w http.ResponseWriter, r *http.Request) (authn.Principal, runwatch.Run, bool) {
	p, err := s.Runs.Authenticate(r)
	if err != nil || p.RunID == "" {
		s.log().Debug("bridge credential refused", "path", r.URL.Path, "why", authWhy(err))
		fail(w, http.StatusUnauthorized, wire.ReasonUnauthenticated)
		return p, runwatch.Run{}, false
	}
	run, ok := s.Watch.Live(p.RunID)
	if !ok {
		fail(w, http.StatusForbidden, wire.ReasonRunNotLive)
		return p, run, false
	}
	if !envelope.ValidID(run.Room) {
		fail(w, http.StatusForbidden, wire.ReasonRunHasNoRoom)
		return p, run, false
	}
	return p, run, true
}

// originClient is a run's idempotency scope for a stream.
func originClient(runID string, st wire.Stream) string {
	if st == wire.StreamStatus {
		return "agent:" + runID + ":status"
	}
	return "agent:" + runID
}

func (s *Server) hello(w http.ResponseWriter, r *http.Request) {
	_, run, ok := s.bridgeAuth(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	// Ruling P17: the room's bridge lease, in the log's database so that every replica
	// agrees (review I7). A holder still live and seen within connectedWindow keeps it.
	holder, took, err := s.Log.ClaimBridge(ctx, run.Room, run.ID, connectedWindow, func(_ context.Context, id string) bool {
		_, live := s.Watch.Live(id)
		return live
	})
	switch {
	case errors.Is(err, store.ErrNoRoom):
		// The Room's row lands with its first reconcile: a 5xx, so the bridge retries.
		fail(w, http.StatusServiceUnavailable, wire.ReasonNoRoom)
		return
	case err != nil:
		s.logFailure(w, err, "claim the bridge lease", "room", run.Room, "run", run.ID)
		return
	}
	if !took {
		ev, dup, err := s.Log.Append(ctx, envelope.Draft{RoomID: run.Room, RunID: run.ID,
			Actor: envelope.Actor{Kind: envelope.ActorSystem, ID: brokerActor}, Type: envelope.StateChanged,
			Origin: envelope.OriginBroker, OriginClient: "broker:busy:" + run.ID, OriginSeq: 1,
			Payload: envelope.StatePayload("limit", map[string]any{"reason": "concurrent_run", "running": holder})})
		switch {
		case err != nil:
			s.log().Warn("record a concurrent run", "room", run.Room, "run", run.ID, errAttr(err))
		case !dup:
			s.appended(ctx, ev)
		}
		s.log().Info("room busy", "room", run.Room, "run", run.ID, "holder", holder)
		fail(w, http.StatusConflict, wire.ReasonRoomBusy)
		return
	}
	res := wire.Resume{RoomID: run.Room}
	if res.AfterHarnessSeq, err = s.Log.Cursor(ctx, run.Room, originClient(run.ID, wire.StreamEvents)); err == nil {
		res.AfterStatusSeq, err = s.Log.Cursor(ctx, run.Room, originClient(run.ID, wire.StreamStatus))
	}
	if err != nil {
		s.logFailure(w, err, "read the bridge's cursors", "room", run.Room, "run", run.ID)
		return
	}
	if s.RoomPolicy != nil {
		res.Approvals = s.RoomPolicy(run.Room)
	}
	reply(w, http.StatusOK, res)
}

// decodeStrict decodes exactly one JSON value with no unknown field.
func decodeStrict(r io.Reader, v any) error {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data after the JSON value")
	}
	return nil
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	p, run, ok := s.bridgeAuth(w, r)
	if !ok {
		return
	}
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
		d, reason := s.bridgeDraft(p, run, it)
		if reason != "" {
			s.log().Warn("bridge item refused", "room", run.Room, "run", run.ID, "type", it.Type, "reason", reason)
			fail(w, http.StatusBadRequest, reason)
			return
		}
		drafts[i] = d
	}
	ctx := r.Context()
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
		ev, dup, err := s.Log.AppendAsBridge(ctx, run.ID, d)
		if store.IsDataError(err) {
			// A value PostgreSQL refuses can never be stored: keep the slot with a stub, as
			// for an oversize payload, so the bridge's cursor moves on (review I6).
			s.log().Error("payload refused by the database", "room", run.Room, "run", run.ID, "type", d.Type, errAttr(err))
			d.Payload, d.Redactions = envelope.Must(map[string]any{"refused": true, "type": d.Type}), nil
			ev, dup, err = s.Log.AppendAsBridge(ctx, run.ID, d)
		}
		if err != nil {
			s.logFailure(w, err, "append a bridge event", "room", run.Room, "run", run.ID, "type", d.Type)
			return
		}
		if !dup {
			s.appended(ctx, ev)
		}
		if it := b.Items[i]; it.Stream == wire.StreamEvents {
			ack.AfterHarnessSeq = max(ack.AfterHarnessSeq, it.Seq)
		} else {
			ack.AfterStatusSeq = max(ack.AfterStatusSeq, it.Seq)
		}
	}
	reply(w, http.StatusOK, ack)
}

// bridgeDraft redacts and checks one pushed item. reason is empty for an item
// the bridge may push.
func (s *Server) bridgeDraft(p authn.Principal, run runwatch.Run, it wire.Item) (envelope.Draft, string) {
	if it.Seq <= 0 || (it.Stream != wire.StreamEvents && it.Stream != wire.StreamStatus) || !it.Type.Valid() {
		return envelope.Draft{}, wire.ReasonBadItem
	}
	payload, rules, err := s.Redactor.Payload(it.Payload)
	if err != nil {
		return envelope.Draft{}, wire.ReasonBadPayload
	}
	obj, ok := object(payload)
	if !ok {
		return envelope.Draft{}, wire.ReasonBadPayload
	}
	if !unambiguous(it.Payload) || !allowedFromBridge(it.Type, obj) {
		return envelope.Draft{}, wire.ReasonBadItem
	}
	return envelope.Draft{RoomID: run.Room, RunID: run.ID,
		Actor: envelope.Actor{Kind: envelope.ActorAgent, ID: p.ID, Role: run.Role}, Type: it.Type,
		Origin: envelope.OriginHarness, OriginClient: originClient(run.ID, it.Stream), OriginSeq: it.Seq,
		Redactions: rules, Payload: payload}, ""
}

// bridgeKinds are the state_changed kinds a bridge produces: its status tracker and
// mapping (phase 1), its acks (phase 4) and its local decisions (phase 5).
var bridgeKinds = map[string]bool{"harness_status": true, "harness_error": true, "harness_paused": true,
	"harness_event": true, "delivered": true, "interrupted": true, "policy_decision": true, "decision_applied": true}

// allowedFromBridge is what a room bridge may push (review M5): what its own code
// produces, never a verdict, a driver change or a decision it could forge. obj is
// the redacted payload, the one stored.
func allowedFromBridge(t envelope.Type, obj map[string]json.RawMessage) bool {
	switch t {
	case envelope.Turn, envelope.ToolCall, envelope.ToolResult:
		return true
	case envelope.Message:
		kind, ok1 := field(obj, "kind")
		delivery, ok2 := field(obj, "delivery")
		return ok1 && ok2 && kind == string(envelope.KindChat) &&
			(delivery == string(envelope.DeliveryNone) || delivery == "")
	case envelope.StateChanged:
		kind, ok := field(obj, "kind")
		return ok && bridgeKinds[kind]
	}
	return false
}

// object decodes a payload's top level; ok is false for anything but a JSON object.
func object(payload json.RawMessage) (map[string]json.RawMessage, bool) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(payload, &obj); err != nil || obj == nil {
		return nil, false
	}
	return obj, true
}

// field reads a top-level string by its exact key: "" when absent, ok false when
// it is not a string.
func field(obj map[string]json.RawMessage, key string) (string, bool) {
	raw, present := obj[key]
	if !present {
		return "", true
	}
	var v string
	return v, json.Unmarshal(raw, &v) == nil
}

// unambiguous refuses top-level keys that differ only by case or by a NUL. Go's
// decoder folds case and keeps the last match, jsonb keeps both keys, and redaction
// drops NULs from keys and merges them: each reader could see another "kind" than
// the one checked here.
func unambiguous(raw json.RawMessage) bool {
	obj, ok := object(raw)
	if !ok {
		return false
	}
	seen := make(map[string]bool, len(obj))
	for k := range obj {
		n := strings.ToLower(strings.ReplaceAll(k, "\x00", ""))
		if seen[n] {
			return false
		}
		seen[n] = true
	}
	return true
}

// stream is the bridge's one downstream channel (C4 r5: SSE, sandbox-initiated).
// Phase 1 sends only pings; phases 4 and 5 add deliver, interrupt and decision.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	p, run, ok := s.bridgeAuth(w, r)
	if !ok {
		return
	}
	rc := http.NewResponseController(w)
	ctx, done := s.conns.attach(r.Context(), run.ID)
	defer done()
	// The run may have ended between bridgeAuth and attach, and its Drop found no
	// stream to end: check again now that a Drop would find this one.
	if _, live := s.Watch.Live(run.ID); !live {
		fail(w, http.StatusForbidden, wire.ReasonRunNotLive)
		return
	}
	// The token's own expiry bounds the stream (§4); the bridge re-dials with a fresh one.
	end := s.now().Add(maxStreamLife)
	if !p.Expiry.IsZero() && p.Expiry.Before(end) {
		end = p.Expiry
	}
	ctx, stop := context.WithDeadline(ctx, end)
	defer stop()
	every := s.PingEvery
	if every <= 0 {
		every = defaultPing
	}
	tick, stopTick := s.ticker(every)
	defer stopTick()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	for {
		_ = rc.SetWriteDeadline(s.now().Add(streamWriteWait))
		if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
			return
		}
		if err := rc.Flush(); err != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-tick:
		}
	}
}

func (s *Server) ticker(d time.Duration) (<-chan time.Time, func()) {
	if s.Ticker != nil {
		return s.Ticker(d)
	}
	t := time.NewTicker(d)
	return t.C, t.Stop
}
