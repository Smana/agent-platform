// SPDX-License-Identifier: Apache-2.0

// Package humanapi is the broker's human listener, :8080, reached only through
// oauth2-proxy (§3): the UI, the room list, and one WebSocket per open room.
// Every API and WebSocket request is authenticated and admitted by group first;
// a room's reads then go through policy.Allowed. The UI's static files are not:
// oauth2-proxy fronts the listener. Live events come from the fan-out hub.
package humanapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/fanout"
	"github.com/Smana/agent-platform/internal/metrics"
	"github.com/Smana/agent-platform/internal/policy"
	"github.com/Smana/agent-platform/internal/runwatch"
	"github.com/Smana/agent-platform/internal/store"
	"github.com/Smana/agent-platform/internal/wire"
)

// Listener bounds. WriteTimeout is 0 for the WebSocket's sake: every other
// route gets routeTimeout in Routes, and each WebSocket write its own WriteWait.
const (
	readHeaderTimeout = 10 * time.Second
	readTimeout       = 30 * time.Second
	idleTimeout       = 120 * time.Second
	maxHeaderBytes    = 32 << 10 // two JWTs ride in the headers
	routeTimeout      = 30 * time.Second
)

// Authenticator maps a request to a human principal; *authn.Humans implements
// it, Origin check included. authn.ErrForbidden means a foreign Origin.
type Authenticator interface {
	Authenticate(*http.Request) (authn.Principal, error)
}

// Log is the part of the store the API reads.
type Log interface {
	Range(ctx context.Context, roomID string, afterSeq int64, limit int) ([]envelope.Event, error)
	Room(ctx context.Context, id string) (store.RoomState, error)
}

// Hub is the fan-out hub's subscription side; *fanout.Hub implements it.
type Hub interface {
	Subscribe(ctx context.Context, roomID string) (*fanout.Sub, error)
	Unsubscribe(*fanout.Sub)
}

// Runs lists a room's runs; *runwatch.Watcher implements it.
type Runs interface {
	InRoom(room string) []runwatch.Run
}

// ActHandler serves an act frame (phase 4 onwards) and returns its ack.
type ActHandler func(ctx context.Context, p authn.Principal, room *v1alpha1.Room, f wire.ClientFrame) wire.ServerFrame

// T10: markdown is rendered with HTML off, and nothing but this origin may run or load.
const csp = "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; " +
	"frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

// Server is the :8080 API. Humans, Groups, WebClient, Rooms, Namespace, Log, Hub
// and Runs are required; the rest is optional.
type Server struct {
	Humans Authenticator
	Groups policy.Groups
	// WebClient is the web UI's client id, read at use (Ruling AS-a): a session
	// through it is the web UI, which may steer and decide (ruling P18).
	WebClient func() string
	Rooms     client.Reader
	Namespace string
	Log       Log
	Hub       Hub
	Runs      Runs
	Metrics   *metrics.Set
	UI        fs.FS // Task 2.5; nil serves 404
	Acts      ActHandler
	Logger    *slog.Logger

	// Connection bounds; zero takes the default.
	HelloWait time.Duration // the first frame must arrive within it (10 s)
	WriteWait time.Duration // one frame to the socket (10 s)
	PingEvery time.Duration // the keep-alive period (30 s)
	PongWait  time.Duration // a ping's pong must arrive within it (10 s)
	// MaxLifetime caps a connection below its token's expiry (1 h): ZITADEL's
	// tokens live 12 h by default, and this bounds a revoked member's socket.
	MaxLifetime time.Duration

	mu      sync.Mutex
	perUser map[string]int
	perRoom map[string]map[string]int
	conns   map[*context.CancelCauseFunc]struct{}
	closing bool
}

// Routes is the :8080 handler.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	index, assets := http.NotFoundHandler(), http.NotFoundHandler()
	if s.UI != nil {
		ui := s.UI
		index = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.ServeFileFS(w, r, ui, "index.html") })
		assets = http.StripPrefix("/assets", http.FileServerFS(ui))
	}
	mux.Handle("GET /{$}", withCSP(index))
	mux.Handle("GET /r/{id}", withCSP(index))
	mux.Handle("GET /assets/{file}", withCSP(assets))
	mux.HandleFunc("GET /api/rooms", s.listRooms)
	mux.HandleFunc("GET /v1/ws", s.ws)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Every response has a write deadline; a hijacked WebSocket's is cleared
		// by net/http, and each of its frames gets WriteWait instead.
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(routeTimeout))
		w.Header().Set("X-Content-Type-Options", "nosniff")
		// A room's page names the room in its path: no link out may carry it.
		w.Header().Set("Referrer-Policy", "no-referrer")
		mux.ServeHTTP(w, r)
	})
}

func withCSP(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", csp)
		h.ServeHTTP(w, r)
	})
}

// Serve serves the API on ln until ctx ends, then drains for at most drain.
// WebSockets end at once, 1001: their clients re-dial another replica.
func (s *Server) Serve(ctx context.Context, ln net.Listener, drain time.Duration) error {
	if s.Humans == nil || s.WebClient == nil || s.Rooms == nil || s.Log == nil || s.Hub == nil || s.Runs == nil {
		// Unchecked, each would panic on every request instead (review M6).
		return errors.New("humanapi: Humans, WebClient, Rooms, Log, Hub and Runs are required")
	}
	srv := &http.Server{Handler: s.Routes(),
		ReadHeaderTimeout: readHeaderTimeout, ReadTimeout: readTimeout, WriteTimeout: 0,
		IdleTimeout: idleTimeout, MaxHeaderBytes: maxHeaderBytes,
		ErrorLog: slog.NewLogLogger(s.log().Handler(), slog.LevelWarn)}
	srv.RegisterOnShutdown(s.closeAll)
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	select {
	case err := <-served:
		return fmt.Errorf("humanapi: serve %s: %w", ln.Addr(), err)
	case <-ctx.Done():
	}
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), drain)
	defer cancel()
	shut := srv.Shutdown(dctx)
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		shut = errors.Join(shut, err)
	}
	// Shutdown does not wait for hijacked connections: wait for their handlers.
	for s.open() > 0 && dctx.Err() == nil {
		select {
		case <-dctx.Done():
		case <-time.After(10 * time.Millisecond):
		}
	}
	if shut != nil {
		return fmt.Errorf("humanapi: shut down %s: %w", ln.Addr(), shut)
	}
	return nil
}

func (s *Server) log() *slog.Logger {
	if s.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.Logger
}

// principal authenticates and admits r, or writes the refusal.
func (s *Server) principal(w http.ResponseWriter, r *http.Request) (authn.Principal, bool) {
	p, err := s.Humans.Authenticate(r)
	if err == nil && !p.Expiry.After(time.Now()) {
		// Inside the verifier's leeway: already past min(exp, 1 h) (review M9).
		err = fmt.Errorf("%w: expired", authn.ErrUnauthenticated)
	}
	if err != nil {
		code := http.StatusUnauthorized
		if errors.Is(err, authn.ErrForbidden) {
			code = http.StatusForbidden
		}
		http.Error(w, http.StatusText(code), code)
		return p, false
	}
	if !s.Groups.Admitted(p) {
		http.Error(w, "not in an agents group", http.StatusForbidden)
		return p, false
	}
	return p, true
}

// room reads a Room CR of this namespace; found is false for none.
func (s *Server) room(ctx context.Context, id string) (room *v1alpha1.Room, found bool, err error) {
	if !envelope.ValidID(id) {
		return nil, false, nil
	}
	room = &v1alpha1.Room{}
	err = s.Rooms.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: id}, room)
	if apierrors.IsNotFound(err) {
		return nil, false, nil
	}
	return room, err == nil, err
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// The metrics' kind and reasons: bounded label values.
const (
	kindHuman = "human"

	dropReauth         = "reauth"
	dropSlowConsumer   = "slow_consumer"
	dropWriteTimeout   = "write_timeout" // a live peer took no frame within WriteWait
	dropPingTimeout    = "ping_timeout"
	dropShutdown       = "shutdown"
	dropLogUnavailable = "log_unavailable"
	dropProtocol       = "protocol" // a frame the client must not send (1007, 1008 hello first, 1009), or no hello
	// dropClientGone is the peer leaving: not a drop, so never counted.
	dropClientGone = "client_gone"
)

func (s *Server) count(ctx context.Context, delta int64) {
	if s.Metrics != nil {
		s.Metrics.Connections.Add(ctx, delta, metric.WithAttributes(attribute.String("kind", kindHuman)))
	}
}

func (s *Server) dropped(ctx context.Context, reason string) {
	if s.Metrics != nil {
		s.Metrics.Dropped.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", reason)))
	}
}

func (s *Server) lag(ctx context.Context, ev envelope.Event) {
	if s.Metrics != nil {
		s.Metrics.FanoutLag.Record(ctx, time.Since(ev.TS).Seconds())
	}
}
