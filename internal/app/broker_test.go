// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/config"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/fanout"
	"github.com/Smana/agent-platform/internal/humanapi"
	"github.com/Smana/agent-platform/internal/metrics"
	"github.com/Smana/agent-platform/internal/policy"
	"github.com/Smana/agent-platform/internal/runwatch"
	"github.com/Smana/agent-platform/internal/store"
)

func TestRunBrokerRefuses(t *testing.T) {
	dir := t.TempDir()
	env := func(kv ...string) func(string) string {
		m := map[string]string{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return func(k string) string { return m[k] }
	}
	for _, c := range []struct {
		name   string
		args   []string
		getenv func(string) string
		want   string
	}{
		{"an unknown subcommand", []string{"serve-all"}, env(), "unknown subcommand"},
		{"serve without its environment names every missing variable", nil, env(),
			"ROOMS_CONFIG is not set\nROOMS_DATABASE_URL is not set\nPOD_NAMESPACE is not set"},
		{"serve with a config it cannot read", []string{"serve"},
			env("ROOMS_CONFIG", filepath.Join(dir, "absent.yaml"), "ROOMS_DATABASE_URL", "postgres://x", "POD_NAMESPACE", "agent-system"),
			"config"},
		{"retention without a database", []string{"retention"}, env(), "ROOMS_DATABASE_URL is not set"},
		{"retention with a database that does not answer", []string{"retention"},
			env("ROOMS_DATABASE_URL", "postgres://rooms_retention@127.0.0.1:1/rooms?connect_timeout=1"), "list expired rooms"},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			err := RunBroker(ctx, slog.New(slog.DiscardHandler), c.args, c.getenv)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("RunBroker = %v, want an error naming %q", err, c.want)
			}
		})
	}
}

type fakeAppends struct {
	ev  envelope.Event
	dup bool
	err error
}

func (f fakeAppends) Append(context.Context, envelope.Draft) (envelope.Event, bool, error) {
	return f.ev, f.dup, f.err
}

func (f fakeAppends) AppendAsBridge(context.Context, string, envelope.Draft) (envelope.Event, bool, error) {
	return f.ev, f.dup, f.err
}

func scrape(t *testing.T, h http.Handler) string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	return rec.Body.String()
}

// RoomLogAppendErrors pages on a failing database, never on an append refused
// for what it is; rooms_events_appended_total counts every new event, whoever wrote it.
func TestMeteredLog(t *testing.T) {
	valid := envelope.Draft{RoomID: "3kq7x2ma", Actor: envelope.Actor{Kind: envelope.ActorSystem, ID: "system:room-broker"},
		Type: envelope.StateChanged, Origin: envelope.OriginBroker, OriginClient: "broker:room", OriginSeq: 1,
		Payload: []byte(`{"kind":"room_phase"}`)}
	stored := envelope.Event{Type: envelope.Message, Origin: envelope.OriginHarness, Redactions: []string{"jwt", "github-pat"}}
	wrapped := func(err error) error { return fmt.Errorf("store: append to room 3kq7x2ma: %w", err) }
	for _, c := range []struct {
		name   string
		draft  envelope.Draft
		fake   fakeAppends
		want   []string
		absent []string
	}{
		{"a new event is counted with its redactions", valid, fakeAppends{ev: stored},
			[]string{`rooms_events_appended_total{origin="harness",type="message"} 1`, `rooms_redactions_total{rule="jwt"} 1`,
				`rooms_redactions_total{rule="github-pat"} 1`, `rooms_append_seconds_count 1`},
			[]string{"rooms_append_errors_total"}},
		{"a replayed key is not a new event", valid, fakeAppends{ev: stored, dup: true},
			[]string{`rooms_append_seconds_count 1`}, []string{"rooms_events_appended_total", "rooms_redactions_total"}},
		{"a failing database is an append error", valid, fakeAppends{err: wrapped(errors.New("conn reset"))},
			[]string{`rooms_append_errors_total 1`, `rooms_append_seconds_count 1`}, []string{"rooms_events_appended_total"}},
		{"a database timeout is an append error", valid, fakeAppends{err: wrapped(context.DeadlineExceeded)},
			[]string{`rooms_append_errors_total 1`}, nil},
		{"a missing room is not", valid, fakeAppends{err: wrapped(store.ErrNoRoom)}, nil, []string{"rooms_append_errors_total"}},
		{"a sealed room is not", valid, fakeAppends{err: wrapped(store.ErrSealed)}, nil, []string{"rooms_append_errors_total"}},
		{"a lost lease is not", valid, fakeAppends{err: wrapped(store.ErrLeaseLost)}, nil, []string{"rooms_append_errors_total"}},
		{"a value PostgreSQL refuses is not", valid, fakeAppends{err: wrapped(&pgconn.PgError{Code: "22P05"})}, nil,
			[]string{"rooms_append_errors_total"}},
		{"a caller that went away is not", valid, fakeAppends{err: wrapped(context.Canceled)}, nil, []string{"rooms_append_errors_total"}},
		{"an invalid draft is not", envelope.Draft{RoomID: "3kq7x2ma"}, fakeAppends{err: errors.New("the actor is stamped by the broker")},
			nil, []string{"rooms_append_errors_total"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			exp, err := metrics.NewExporter("test")
			if err != nil {
				t.Fatal(err)
			}
			m, err := metrics.New(exp.Meter())
			if err != nil {
				t.Fatal(err)
			}
			l := &meteredLog{appends: c.fake, m: m, now: time.Now}
			for i, appendOnce := range []func() error{
				func() error { _, _, err := l.Append(t.Context(), c.draft); return err },
				func() error { _, _, err := l.AppendAsBridge(t.Context(), "7f3cq2xz", c.draft); return err },
			} {
				if err := appendOnce(); !errors.Is(err, c.fake.err) {
					t.Fatalf("append %d returned %v, want the store's %v", i, err, c.fake.err)
				}
			}
			// Both writes count the same way: each want line holds once per write.
			body := scrape(t, exp.Handler())
			for _, w := range c.want {
				w = strings.Replace(w, "} 1", "} 2", 1)
				w = strings.Replace(w, "_count 1", "_count 2", 1)
				w = strings.Replace(w, "_total 1", "_total 2", 1)
				if !strings.Contains(body, w+"\n") {
					t.Errorf("missing %s", w)
				}
			}
			for _, a := range c.absent {
				if strings.Contains(body, a+" ") || strings.Contains(body, a+"{") {
					t.Errorf("exposes %s", a)
				}
			}
			if t.Failed() {
				t.Log(body)
			}
		})
	}
}

// Ruling AP: nothing scrapes a sandbox, so the broker counts the stalls and stubs
// its bridges tell the rooms, as it appends them. Labels stay bounded: the
// harness's own error codes and kinds are not counted.
func TestBridgeSignals(t *testing.T) {
	state := func(fields string) json.RawMessage { return json.RawMessage(`{"kind":` + fields + `}`) }
	for _, c := range []struct {
		name    string
		typ     envelope.Type
		origin  envelope.Origin
		payload json.RawMessage
		want    string // "" means neither counter moves
	}{
		{"a stall notice counts its code", envelope.StateChanged, envelope.OriginHarness,
			state(`"harness_error","code":"cursor_lost","detail":"the bridge cannot read past this point"`),
			`rooms_bridge_harness_stalls_total{reason="cursor_lost"} 1`},
		{"every stall reason", envelope.StateChanged, envelope.OriginHarness,
			state(`"harness_error","code":"next_page_unreadable"`), `rooms_bridge_harness_stalls_total{reason="next_page_unreadable"} 1`},
		{"the harness's own error is not a stall", envelope.StateChanged, envelope.OriginHarness,
			state(`"harness_error","code":"LLMRateLimit","detail":"429"`), ""},
		{"a refused item's stub", envelope.StateChanged, envelope.OriginHarness,
			state(`"harness_event","harnessKind":"refused","detail":"message","bytes":12,"reason":"bad_payload"`),
			`rooms_bridge_items_stubbed_total{reason="refused"} 1`},
		{"an oversize item's stub", envelope.StateChanged, envelope.OriginHarness,
			state(`"harness_event","harnessKind":"oversize","detail":"message","bytes":3000000`),
			`rooms_bridge_items_stubbed_total{reason="oversize"} 1`},
		{"an oversize tool result, stubbed by the bridge or the store", envelope.ToolResult, envelope.OriginHarness,
			json.RawMessage(`{"oversize":true,"bytes":70000,"type":"tool_result"}`), `rooms_bridge_items_stubbed_total{reason="oversize"} 1`},
		// S1 review I-4: each stub says why, in a bounded set of reasons.
		{"the broker's stub for a value the store rejected", envelope.Message, envelope.OriginHarness,
			json.RawMessage(`{"reason":"invalid_value","refused":true,"type":"message"}`), `rooms_bridge_items_stubbed_total{reason="invalid_value"} 1`},
		{"the broker's stub for keys that collide once redacted", envelope.ToolResult, envelope.OriginHarness,
			json.RawMessage(`{"reason":"key_collision","refused":true,"type":"tool_result"}`), `rooms_bridge_items_stubbed_total{reason="key_collision"} 1`},
		{"a stub with a reason outside the set is not counted", envelope.Message, envelope.OriginHarness,
			json.RawMessage(`{"reason":"whatever","refused":true,"type":"message"}`), ""},
		{"an unknown harness kind is not a stub", envelope.StateChanged, envelope.OriginHarness,
			state(`"harness_event","harnessKind":"CondensationEvent"`), ""},
		{"a malformed harness event is not a stub", envelope.StateChanged, envelope.OriginHarness,
			state(`"harness_event","harnessKind":"malformed"`), ""},
		{"only bridges' events count", envelope.StateChanged, envelope.OriginBroker,
			state(`"harness_error","code":"cursor_lost"`), ""},
		{"an ordinary event", envelope.Message, envelope.OriginHarness,
			json.RawMessage(`{"kind":"chat","text":"\"oversize\":true","delivery":"none"}`), ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			exp, err := metrics.NewExporter("test")
			if err != nil {
				t.Fatal(err)
			}
			m, err := metrics.New(exp.Meter())
			if err != nil {
				t.Fatal(err)
			}
			ev := envelope.Event{Type: c.typ, Origin: c.origin, Payload: c.payload}
			l := &meteredLog{appends: fakeAppends{ev: ev}, m: m, now: time.Now}
			if _, _, err := l.AppendAsBridge(t.Context(), "7f3cq2xz", envelope.Draft{}); err != nil {
				t.Fatal(err)
			}
			// A replay of the same key is not a second stall.
			l.appends = fakeAppends{ev: ev, dup: true}
			if _, _, err := l.AppendAsBridge(t.Context(), "7f3cq2xz", envelope.Draft{}); err != nil {
				t.Fatal(err)
			}
			body := scrape(t, exp.Handler())
			for _, name := range []string{"rooms_bridge_harness_stalls_total", "rooms_bridge_items_stubbed_total"} {
				if (c.want == "" || !strings.HasPrefix(c.want, name)) && strings.Contains(body, name+"{") {
					t.Errorf("counts %s:\n%s", name, body)
				}
			}
			if c.want != "" && !strings.Contains(body, c.want+"\n") {
				t.Errorf("missing %s:\n%s", c.want, body)
			}
		})
	}
}

func TestLeader(t *testing.T) {
	for _, c := range []struct {
		name   string
		synced bool
		ticks  int
		sweeps int
		replay int
	}{
		{"it replays once and sweeps at once and on every tick", true, 2, 3, 1},
		{"it waits for the watch: an unsynced one is never swept", false, 0, 0, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			var active atomic.Bool
			var mu sync.Mutex
			var replays, sweeps int
			var activeWhile []bool
			swept := make(chan struct{}, 10)
			tick := make(chan time.Time)
			l := &leader{active: &active, every: time.Hour,
				// Review M5: marked leader before the wait, so what the informer
				// delivers during the sync is appended by its callback.
				synced: func(context.Context) bool {
					mu.Lock()
					defer mu.Unlock()
					activeWhile = append(activeWhile, active.Load())
					return c.synced
				},
				replay: func(context.Context) {
					mu.Lock()
					defer mu.Unlock()
					replays++
					activeWhile = append(activeWhile, active.Load())
				},
				sweep: func(context.Context) {
					mu.Lock()
					sweeps++
					activeWhile = append(activeWhile, active.Load())
					mu.Unlock()
					swept <- struct{}{}
				},
				ticker: func(d time.Duration) (<-chan time.Time, func()) {
					if d != time.Hour {
						t.Errorf("ticker period %s", d)
					}
					return tick, func() {}
				}}
			if !l.NeedLeaderElection() {
				t.Fatal("only the leader appends run lifecycles")
			}
			runCtx, stop := context.WithCancel(ctx)
			done := make(chan error, 1)
			go func() { done <- l.Start(runCtx) }()
			if c.synced {
				<-swept
				for range c.ticks {
					tick <- time.Now()
					<-swept
				}
			}
			stop()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("Start did not return when leadership ended")
			}
			mu.Lock()
			defer mu.Unlock()
			if replays != c.replay || sweeps != c.sweeps {
				t.Fatalf("replays=%d sweeps=%d, want %d and %d", replays, sweeps, c.replay, c.sweeps)
			}
			if len(activeWhile) != 1+c.replay+c.sweeps {
				t.Fatalf("%d calls recorded, want the sync wait and every replay and sweep", len(activeWhile))
			}
			for i, a := range activeWhile {
				if !a {
					t.Fatalf("call %d (0 is the sync wait) ran while not marked leader", i)
				}
			}
			if active.Load() {
				t.Fatal("still marked leader after Start returned")
			}
		})
	}
}

// Ledger 1.7 (a): a stalled database must not freeze the watch's handler. The
// parent has no deadline (review I2), so only bounded's own can be the one seen.
func TestBounded(t *testing.T) {
	run := runwatch.Run{ID: "7f3cq2xz", Room: "3kq7x2ma"}
	for _, c := range []struct {
		name    string
		leading bool
		called  bool
	}{
		{"a follower appends nothing", false, false},
		{"the leader's append is bounded", true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			var leading atomic.Bool
			leading.Store(c.leading)
			called := false
			var deadline time.Time
			f := bounded(&leading, slog.New(slog.DiscardHandler), "test", func(ctx context.Context, r runwatch.Run) error {
				called = true
				deadline, _ = ctx.Deadline()
				if r.ID != run.ID || r.Room != run.Room {
					t.Errorf("got run %+v", r)
				}
				return errors.New("stalled")
			})
			if _, has := t.Context().Deadline(); has {
				t.Fatal("the parent must have no deadline, or the test proves nothing")
			}
			f(t.Context(), run)
			if called != c.called {
				t.Fatalf("called=%v want %v", called, c.called)
			}
			if !c.called {
				return
			}
			if left := time.Until(deadline); deadline.IsZero() || left <= observeTimeout-time.Second || left > observeTimeout {
				t.Fatalf("the append's deadline is %v away, want about %s", left, observeTimeout)
			}
		})
	}
}

func TestOps(t *testing.T) {
	up := func(context.Context) error { return nil }
	down := func(context.Context) error { return errors.New("refused") }
	migrated := func(context.Context) (bool, error) { return true, nil }
	unmigrated := func(context.Context) (bool, error) { return false, nil }
	broken := func(context.Context) (bool, error) { return false, errors.New("refused") }
	metricsH := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("rooms_build_info 1")) })
	for _, c := range []struct {
		name     string
		path     string
		ping     func(context.Context) error
		schema   func(context.Context) (bool, error)
		synced   bool
		draining bool
		want     int
	}{
		{"healthz answers while the process does", "/healthz", down, broken, false, true, http.StatusOK},
		{"ready once the database answers and the watch synced", "/readyz", up, migrated, true, false, http.StatusOK},
		{"not ready while the database does not answer", "/readyz", down, migrated, true, false, http.StatusServiceUnavailable},
		{"not ready before the watch synced", "/readyz", up, migrated, false, false, http.StatusServiceUnavailable},
		{"not ready while draining, so no new work is routed here", "/readyz", up, migrated, true, true, http.StatusServiceUnavailable},
		{"started once the schema is migrated", "/startupz", up, migrated, true, false, http.StatusOK},
		{"not started before Atlas ran", "/startupz", up, unmigrated, true, false, http.StatusServiceUnavailable},
		{"not started while the schema cannot be read", "/startupz", up, broken, true, false, http.StatusServiceUnavailable},
		{"metrics are served, while draining too", "/metrics", down, broken, false, true, http.StatusOK},
		{"nothing else", "/debug/pprof/", up, migrated, true, false, http.StatusNotFound},
	} {
		t.Run(c.name, func(t *testing.T) {
			synced := boundedSync(func(ctx context.Context) bool {
				if dl, ok := ctx.Deadline(); !ok || time.Until(dl) > syncCheck {
					t.Error("readiness must bound its wait on the run watch")
				}
				return c.synced
			})
			h := opsHandler(c.ping, c.schema, synced, func() bool { return c.draining }, metricsH)
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, c.path, nil))
			if rec.Code != c.want {
				t.Fatalf("GET %s = %d, want %d", c.path, rec.Code, c.want)
			}
		})
	}
	srv := opsServer(http.NotFoundHandler(), slog.New(slog.DiscardHandler))
	if srv.ReadHeaderTimeout == 0 || srv.ReadTimeout == 0 || srv.WriteTimeout == 0 || srv.IdleTimeout == 0 || srv.MaxHeaderBytes == 0 {
		t.Fatalf(":9090 streams nothing, so every bound is set: %+v", srv)
	}
}

func TestServeHTTPStopsWithItsContext(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	runCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- serveHTTP(runCtx, srv.Config, srv.Listener, time.Second) }()
	stop()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("serveHTTP did not return when its context ended")
	}
}

type fakeRoomLog struct{}

func (fakeRoomLog) Range(context.Context, string, int64, int) ([]envelope.Event, error) {
	return nil, nil
}

func (fakeRoomLog) Room(context.Context, string) (store.RoomState, error) {
	return store.RoomState{}, nil
}

func (fakeRoomLog) Queue(context.Context, string) ([]store.Queued, error) { return nil, nil }

type fakeRuns struct{}

func (fakeRuns) InRoom(string) []runwatch.Run { return nil }

// :8080 gets the config's groups, the web client read at use (Ruling AS-a), the
// embedded UI, the metrics, and every part Serve requires.
func TestHumanServer(t *testing.T) {
	log := slog.New(slog.DiscardHandler)
	clientFile := filepath.Join(t.TempDir(), "client-id")
	writeID(t, clientFile, "web-1")
	h := config.HumanConfig{ClientIDFile: clientFile, Groups: config.GroupsConfig{Admin: "agents-admin", Member: "agents-member"}}
	humans := authn.NewHumans(authn.NewVerifierWithKeyfunc(humanIssuer, nil), idFile(""), idFile(""), idFile(""), "https://rooms.example.test")
	m, err := metrics.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	actor := &humanapi.Actor{}
	s := humanServer(h, humans, fake.NewClientBuilder().Build(), "agent-system", fakeRoomLog{},
		fanout.New(fakeRoomLog{}, nil, log), fakeRuns{}, actor, m, log)
	if s.Groups != (policy.Groups{Admin: "agents-admin", Member: "agents-member"}) || s.Namespace != "agent-system" || s.Metrics != m ||
		s.Actor != actor {
		t.Fatalf("groups %+v, namespace %q, metrics %p, actor %p", s.Groups, s.Namespace, s.Metrics, s.Actor)
	}
	writeID(t, clientFile, "web-2")
	if got := s.WebClient(); got != "web-2" {
		t.Fatalf("web client %q, want the file's current id", got)
	}
	rec := httptest.NewRecorder()
	s.Routes().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<html") {
		t.Fatalf("GET / = %d, want the UI's index.html", rec.Code)
	}
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := s.Serve(ctx, ln, time.Second); err != nil {
		t.Fatalf("Serve refused its parts: %v", err)
	}
}

// The hub's listener gauge is exported from the start (FORWARD 2.6).
func TestFanoutHub(t *testing.T) {
	exp, err := metrics.NewExporter("test")
	if err != nil {
		t.Fatal(err)
	}
	m, err := metrics.New(exp.Meter())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fanoutHub(fakeRoomLog{}, nil, m, slog.New(slog.DiscardHandler)); err != nil {
		t.Fatal(err)
	}
	if body := scrape(t, exp.Handler()); !strings.Contains(body, "rooms_fanout_listener_up 0\n") {
		t.Fatalf("no listener gauge in\n%s", body)
	}
}

// The pod's 30 s grace holds the parallel drains, then :9090's and the metrics
// flush. :8080's covers a write blocked for WriteWait (10 s) plus coder/websocket's
// 5 s close handshake (review M11).
func TestDrainsFitTheGrace(t *testing.T) {
	const podGrace = 30 * time.Second
	if humanDrain < 15*time.Second {
		t.Fatalf(":8080 drains for %s, want at least 15 s", humanDrain)
	}
	if d := max(bridgeDrain, managerDrain, humanDrain, mcpDrain) + opsDrain + metricsDrain; d >= podGrace {
		t.Fatalf("the drains take %s, want under the %s grace", d, podGrace)
	}
}
