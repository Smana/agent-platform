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
	"strconv"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/fanout"
	"github.com/Smana/agent-platform/internal/runwatch"
	"github.com/Smana/agent-platform/internal/store"
	"github.com/Smana/agent-platform/internal/wire"
)

const (
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
	// defaultStreamWriteWait bounds one write to a stream, so a bridge that stops
	// reading cannot pin a handler until its token expires. It is armed only
	// while a write and its flush are under way.
	defaultStreamWriteWait = 10 * time.Second

	brokerActor = "system:room-broker"
)

// defaultReplayPage is how many deliveries a stream's replay reads at once when
// Server.ReplayPage is unset.
const defaultReplayPage = 500

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
	// Deliveries is the run's deliverable events in (after, through], paged (phase 4).
	Deliveries(ctx context.Context, roomID, runID string, after, through int64, limit int) ([]envelope.Event, error)
	Cursor(ctx context.Context, roomID, originClient string) (int64, error)
	Room(ctx context.Context, id string) (store.RoomState, error)
	ClaimBridge(ctx context.Context, roomID, runID string, live func(ctx context.Context, runID string) bool) (string, bool, error)
	TouchBridge(ctx context.Context, roomID, runID string) (held bool, err error)
}

// Redactor removes secrets from a JSON payload, keys included, and fails once
// ctx ends rather than return it partly scanned; *redact.Redactor implements it.
type Redactor interface {
	Payload(ctx context.Context, raw json.RawMessage) (json.RawMessage, []string, error)
}

// Hub is the fan-out hub's subscription side; *fanout.Hub implements it.
type Hub interface {
	Subscribe(ctx context.Context, roomID string) (*fanout.Sub, error)
	Unsubscribe(*fanout.Sub)
}

// Liveness answers whether a run is live; *runwatch.Watcher implements it.
type Liveness interface {
	Live(runID string) (run runwatch.Run, live, known bool)
}

// Server is the :8443 API. Log, Redactor, Runs, Systems and Watch are required;
// the rest is optional.
type Server struct {
	Log      Log
	Redactor Redactor
	Runs     Authenticator
	Systems  Authenticator
	Watch    Liveness
	// RoomPolicy, when set, is the room's approval policy handed out at hello and
	// read for each approval's deadline (phase 5); unset means attended.
	RoomPolicy func(ctx context.Context, roomID string) wire.ApprovalPolicy
	// Approvals records approval requests (phase 5); unset answers 503.
	Approvals Approvals
	// PingEvery is the stream's keep-alive period; 0 means 30 s.
	PingEvery time.Duration
	// Ticker starts the stream's keep-alive; nil means a time.Ticker.
	Ticker func(d time.Duration) (c <-chan time.Time, stop func())
	// ReplayPage is how many deliveries a stream's replay reads at once; 0 means 500.
	ReplayPage int
	// StreamWriteWait bounds each write to a stream; 0 means 10 s.
	StreamWriteWait time.Duration
	// Hub feeds each stream its room's live events, and LastAck is where a run's
	// deliveries resume (phase 4). The stream refuses to start without them.
	Hub     Hub
	LastAck func(ctx context.Context, room, run string) (int64, error)
	// Limits bound each principal on the events endpoint and the system API.
	Limits Limits
	// Queue serves the system queue routes (SP3 R9); nil answers them 501.
	Queue QueueStore
	// SlackLinks serves GET /v1/links/slack to SlackLinkReader alone (the factory, system:factory);
	// either unset refuses it.
	SlackLinks      SlackLinks
	SlackLinkReader string
	// Now is the limits' clock; nil means time.Now. Deadlines handed to the
	// runtime (a context's, a connection's) stay on its own clock: a fake one
	// there would expire a request before it starts, or never.
	Now    func() time.Time
	Logger *slog.Logger

	conns     registry
	limitOnce sync.Once
	limit     *limiter

	reviewWarned sync.Map // cause → time.Time of its last Warn
}

// Routes is the :8443 handler.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/bridge/hello", s.bounded(0, s.hello))
	mux.HandleFunc("POST /v1/bridge/events", s.bounded(maxBatchBytes, s.events))
	mux.HandleFunc("GET /v1/bridge/stream", s.stream)
	mux.HandleFunc("POST /v1/bridge/approvals", s.bounded(maxApprovalBytes, s.approval))
	mux.HandleFunc("GET /v1/rooms/{id}/events", s.bounded(0, s.roomEvents))
	mux.HandleFunc("POST /v1/rooms/{id}/messages", s.bounded(maxMessageBytes, s.roomMessage))
	mux.HandleFunc("POST /v1/rooms/{id}/task", s.bounded(maxMessageBytes, s.roomTask))
	mux.HandleFunc("POST /v1/rooms/{id}/queue", s.bounded(maxMessageBytes, s.queueRoute(s.enqueue)))
	mux.HandleFunc("GET /v1/rooms/{id}/queue", s.bounded(0, s.queueRoute(s.listQueue)))
	mux.HandleFunc("POST /v1/rooms/{id}/queue/consume", s.bounded(maxConsumeBytes, s.queueRoute(s.consume)))
	mux.HandleFunc("GET /v1/links/slack", s.bounded(0, s.slackLink))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Every response, the mux's own 404 and 405 included, has a write deadline;
		// the stream replaces it with one per write.
		// ErrNotSupported only from a writer that is not a connection (a test's recorder).
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(routeTimeout))
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

// admit applies principal's limits, answering 429 when it is over them. release
// is called once the request is served.
func (s *Server) admit(w http.ResponseWriter, principal string) (release func(), ok bool) {
	s.limitOnce.Do(func() { s.limit = newLimiter(s.Limits, s.now) })
	release, ok = s.limit.acquire(principal)
	if !ok {
		// Debug: under abuse this line would be as unbounded as the requests it refuses.
		s.log().Debug("principal over its limits", "principal", principal)
		w.Header().Set("Retry-After", strconv.Itoa(1))
		fail(w, http.StatusTooManyRequests, wire.ReasonRateLimited)
	}
	return release, ok
}

// bounded caps a non-streaming route's body and its work; Routes caps its response.
func (s *Server) bounded(maxBody int64, h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), routeTimeout)
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

// warnEvery spaces the Warn for one unchanged cause: a bridge retries a 401 forever.
const warnEvery = time.Minute

// warnReview logs a TokenReview call that failed, with its cause (never the token), at Warn: it is
// the broker's RBAC or the apiserver, not a bad credential. Once a minute per distinct cause.
func (s *Server) warnReview(err error) {
	cause := err.Error()
	now := s.now()
	if last, ok := s.reviewWarned.Load(cause); ok && now.Sub(last.(time.Time)) < warnEvery {
		return
	}
	s.reviewWarned.Store(cause, now)
	s.log().Warn("token review failed: check the broker's system:auth-delegator binding and the apiserver", "err", cause)
}

// bridgeAuth admits a run token for a run that is live and names a room (§1 Admission).
func (s *Server) bridgeAuth(w http.ResponseWriter, r *http.Request) (authn.Principal, runwatch.Run, bool) {
	p, err := s.Runs.Authenticate(r)
	if err != nil || p.RunID == "" {
		if errors.Is(err, authn.ErrReviewUnavailable) {
			s.warnReview(err)
		} else {
			s.log().Debug("bridge credential refused", "path", r.URL.Path, "why", authWhy(err))
		}
		fail(w, http.StatusUnauthorized, wire.ReasonUnauthenticated)
		return p, runwatch.Run{}, false
	}
	run, live, _ := s.Watch.Live(p.RunID)
	if !live {
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
	p, run, ok := s.bridgeAuth(w, r)
	if !ok {
		return
	}
	// Each hello runs a ClaimBridge transaction: it shares the run's limits.
	release, ok := s.admit(w, p.ID)
	if !ok {
		return
	}
	defer release()
	ctx := r.Context()
	// Ruling P17: the room's bridge lease, in the log's database so that every replica
	// agrees (review I7). A holder keeps it while its run is live (ruling SBB).
	holder, took, err := s.Log.ClaimBridge(ctx, run.Room, run.ID, func(_ context.Context, id string) bool {
		// An unknown holder is kept until this replica's run cache has synced
		// (review 2026-10-15, B8).
		_, live, known := s.Watch.Live(id)
		return live || !known
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
		_, _, err := s.Log.Append(ctx, envelope.Draft{RoomID: run.Room, RunID: run.ID,
			Actor: envelope.Actor{Kind: envelope.ActorSystem, ID: brokerActor}, Type: envelope.StateChanged,
			Origin: envelope.OriginBroker, OriginClient: runwatch.BusyScope + run.ID, OriginSeq: 1,
			Payload: envelope.StatePayload("limit", map[string]any{"reason": "concurrent_run", "running": holder})})
		if err != nil {
			s.log().Warn("record a concurrent run", "room", run.Room, "run", run.ID, errAttr(err))
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
		res.Approvals = s.RoomPolicy(ctx, run.Room)
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
