// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/sync/errgroup"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/bridgeapi"
	"github.com/Smana/agent-platform/internal/config"
	"github.com/Smana/agent-platform/internal/fanout"
	"github.com/Smana/agent-platform/internal/humanapi"
	"github.com/Smana/agent-platform/internal/humanapi/ui"
	"github.com/Smana/agent-platform/internal/metrics"
	"github.com/Smana/agent-platform/internal/policy"
	"github.com/Smana/agent-platform/internal/redact"
	"github.com/Smana/agent-platform/internal/roomctrl"
	"github.com/Smana/agent-platform/internal/runwatch"
	"github.com/Smana/agent-platform/internal/store"
	"github.com/Smana/agent-platform/internal/version"
)

// The broker's listeners and bounds (§9 ports; GP-18 for :8443).
const (
	bridgeAddr = ":8443"
	humanAddr  = ":8080"
	opsAddr    = ":9090"
	// observeTimeout bounds one run's lifecycle append from the informer's
	// handler: a stalled database must not freeze the liveness mirror, which
	// every replica's bridge admission reads.
	observeTimeout = 10 * time.Second
	// sweepEvery and sweepTimeout pace the leader's "joined, never left" sweep.
	sweepEvery   = 5 * time.Minute
	sweepTimeout = time.Minute
	// The drains fit the pod's 30 s grace: the manager, :8443, :8080 and :8090 in
	// parallel, then :9090 and the metrics provider. :8080's covers a WebSocket
	// write blocked for WriteWait (10 s) plus the 5 s close handshake (review M11).
	bridgeDrain  = 20 * time.Second
	managerDrain = 20 * time.Second
	humanDrain   = 20 * time.Second
	opsDrain     = 2 * time.Second
	metricsDrain = time.Second
)

// RunBroker runs room-broker's subcommand args[0]: serve (the default) or
// retention. getenv reads ROOMS_CONFIG, ROOMS_DATABASE_URL, POD_NAMESPACE,
// ROOMS_MCP_KEY and ROOMS_GITHUB_APP_DIR.
func RunBroker(ctx context.Context, log *slog.Logger, args []string, getenv func(string) string) error {
	cmd := "serve"
	if len(args) > 0 {
		cmd = args[0]
	}
	switch cmd {
	case "serve":
		return serveBroker(ctx, log, getenv)
	case "retention":
		return runRetention(ctx, log, getenv)
	}
	return fmt.Errorf("room-broker: unknown subcommand %q (serve, retention)", cmd)
}

// runRetention is the daily DELETE-only job (§4, OD-17), connected as
// rooms_retention.
func runRetention(ctx context.Context, log *slog.Logger, getenv func(string) string) error {
	dsn := getenv("ROOMS_DATABASE_URL")
	if dsn == "" {
		return errors.New("room-broker retention: ROOMS_DATABASE_URL is not set")
	}
	st, err := store.Open(ctx, dsn)
	if err != nil {
		return fmt.Errorf("room-broker retention: %w", err)
	}
	defer st.Close()
	rooms, events, err := st.PurgeExpired(ctx)
	log.Info("purged expired rooms", "rooms", rooms, "events", events)
	if err != nil {
		return fmt.Errorf("room-broker retention: %w", err)
	}
	return nil
}

func brokerEnv(getenv func(string) string) (cfgPath, dsn, ns string, err error) {
	var missing []error
	for _, kv := range []struct {
		name string
		dst  *string
	}{{"ROOMS_CONFIG", &cfgPath}, {"ROOMS_DATABASE_URL", &dsn}, {"POD_NAMESPACE", &ns}} {
		if *kv.dst = getenv(kv.name); *kv.dst == "" {
			missing = append(missing, fmt.Errorf("%s is not set", kv.name))
		}
	}
	return cfgPath, dsn, ns, errors.Join(missing...)
}

// serveBroker runs the Room controller, the AgentRun watch, the :8443 API, the
// :8080 human API and its fan-out hub, the :8090 room tools, the JWKS refresh and the :9090 metrics
// and probes until ctx ends or one of them fails. :9090 stops last, so the drain's final counts are scraped and /readyz
// answers 503 rather than refusing connections meanwhile (review M4).
func serveBroker(ctx context.Context, log *slog.Logger, getenv func(string) string) error {
	cfgPath, dsn, ns, err := brokerEnv(getenv)
	if err != nil {
		return fmt.Errorf("room-broker: %w", err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("room-broker: %w", err)
	}
	tlsCfg, err := bridgeapi.TLSConfig(cfg.TLS.CertFile, cfg.TLS.KeyFile, log)
	if err != nil {
		return fmt.Errorf("room-broker: %w", err)
	}
	exp, err := metrics.NewExporter(metrics.BrokerBuildInfo, version.Version)
	if err != nil {
		return fmt.Errorf("room-broker: %w", err)
	}
	defer shutdownMetrics(ctx, log, exp)
	m, err := metrics.New(exp.Meter())
	if err != nil {
		return fmt.Errorf("room-broker: %w", err)
	}
	st, err := store.Open(ctx, dsn)
	if err != nil {
		return fmt.Errorf("room-broker: %w", err)
	}
	defer st.Close()
	logStore := &meteredLog{Store: st, appends: st, drivers: st, m: m, now: time.Now}
	red, err := redact.New()
	if err != nil {
		return fmt.Errorf("room-broker: redaction rules: %w", err)
	}
	a, err := authenticators(ctx, cfg, m, jwksVerifier(log))
	if err != nil {
		return fmt.Errorf("room-broker: %w", err)
	}

	mgr, err := newManager(log, ns)
	if err != nil {
		return fmt.Errorf("room-broker: %w", err)
	}
	events := &runwatch.Events{Store: logStore, Redactor: red}
	rw, err := wireRuns(ctx, mgr.GetCache(), mgr.Add, log, st, events, nil)
	if err != nil {
		return fmt.Errorf("room-broker: %w", err)
	}
	rc := &roomctrl.Reconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Store: logStore,
		Runs: rw.watch, Ends: events, Log: log, Forget: m.ForgetRoom, Observe: roomObserver(m, rw.watch)}
	if err := rc.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("room-broker: %w", err)
	}

	api, err := bridgeAPI(logStore, red, a.runs, a.systems, rw.watch, mgr.GetClient(), ns, mgr.Add, m, log)
	if err != nil {
		return fmt.Errorf("room-broker: %w", err)
	}
	api.Queue = logStore // the system queue routes (SP3 R9)
	rw.watch.OnGone(api.Drop)
	hub, err := fanoutHub(st, st, m, log)
	if err != nil {
		return fmt.Errorf("room-broker: %w", err)
	}
	api.Hub, api.LastAck = hub, st.LastAck // the stream's deliveries (phase 4)
	humans, err := humanSide(cfg, a.humans, mgr.GetClient(), ns, logStore, red, hub, rw.watch, mgr.Add, m, log)
	if err != nil {
		return fmt.Errorf("room-broker: %w", err)
	}
	key := mcpKey(getenv)
	if key == "" {
		log.Warn("ROOMS_MCP_KEY is not set: :8090 refuses every room tool call")
	}
	tools := roomMCP(key, subPatterns(cfg.RunIssuers), logStore, red, rw.watch, m, log)
	if err := addVerdictPoster(getenv("ROOMS_GITHUB_APP_DIR"), mgr.Add, logStore, mgr.GetClient(), ns, cfg.PublicURL, m, log); err != nil {
		return fmt.Errorf("room-broker: verdict poster: %w", err)
	}
	ops := opsHandler(st.Ping, st.SchemaReady, boundedSync(rw.synced), func() bool { return ctx.Err() != nil }, exp.Handler())

	var lc net.ListenConfig
	humanLn, err := lc.Listen(ctx, "tcp", humanAddr)
	if err != nil {
		return fmt.Errorf("room-broker: human listener: %w", err)
	}
	mcpLn, err := lc.Listen(ctx, "tcp", mcpAddr)
	if err != nil {
		_ = humanLn.Close()
		return fmt.Errorf("room-broker: room tools listener: %w", err)
	}
	opsLn, err := lc.Listen(ctx, "tcp", opsAddr)
	if err != nil {
		_ = humanLn.Close()
		_ = mcpLn.Close()
		return fmt.Errorf("room-broker: ops listener: %w", err)
	}
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	opsCtx, stopOps := context.WithCancel(context.WithoutCancel(ctx))
	defer stopOps()
	opsDone := make(chan error, 1)
	go func() {
		err := serveHTTP(opsCtx, opsServer(ops, log), opsLn, opsDrain)
		if err != nil {
			cancelRun() // :9090 failing takes the rest down: the pod is unprobeable
		}
		opsDone <- err
	}()
	refreshers := make([]refresher, 0, len(a.verifiers))
	for _, v := range a.verifiers {
		refreshers = append(refreshers, v)
	}
	g, gctx := errgroup.WithContext(runCtx)
	g.Go(func() error { return api.ListenAndServeTLS(gctx, bridgeAddr, tlsCfg, bridgeDrain) })
	g.Go(func() error { return hub.Run(gctx) })
	g.Go(func() error { return humans.Serve(gctx, humanLn, humanDrain) })
	g.Go(func() error { return serveHTTP(gctx, mcpServer(tools, mcpCallTimeout, log), mcpLn, mcpDrain) })
	g.Go(func() error { return refreshJWKS(gctx, refreshers, nil) })
	g.Go(func() error {
		if err := mgr.Start(gctx); err != nil {
			return fmt.Errorf("manager: %w", err)
		}
		return nil
	})
	log.Info("room-broker starting", "version", version.Version, "namespace", ns)
	err = g.Wait()
	stopOps()
	if err = errors.Join(err, <-opsDone); err != nil {
		return fmt.Errorf("room-broker: %w", err)
	}
	return nil
}

// fanoutHub is the replica's fan-out hub over the log and its LISTEN session,
// with the listener's gauge (FORWARD 2.6). Every append notifies inside its own
// transaction (Ruling AT), so no writer calls the hub.
func fanoutHub(r fanout.Reader, l fanout.Listener, m *metrics.Set, log *slog.Logger) (*fanout.Hub, error) {
	hub := fanout.New(r, l, log)
	if err := m.WatchFanout(hub.Healthy); err != nil {
		return nil, err
	}
	return hub, nil
}

// humanServer is the :8080 API over the broker's parts, its acts served by
// actor. The web client id is read at use (Ruling AS-a); the group names are literals.
func humanServer(h config.HumanConfig, humans *authn.Humans, rooms client.Reader, ns string, roomLog humanapi.Log,
	hub humanapi.Hub, runs humanapi.Runs, actor *humanapi.Actor, m *metrics.Set, log *slog.Logger,
) *humanapi.Server {
	return &humanapi.Server{Humans: humans, Groups: policy.Groups{Admin: h.Groups.Admin, Member: h.Groups.Member},
		WebClient: idFile(h.ClientIDFile), Rooms: rooms, Namespace: ns, Log: roomLog, Hub: hub, Runs: runs,
		Actor: actor, Metrics: m, UI: ui.FS, Logger: log}
}

// roomRuns is the one watch method roomObserver reads; *runwatch.Watcher has it.
type roomRuns interface {
	InRoom(room string) []runwatch.Run
}

// roomObserver feeds the room gauges from a reconciled status and the watch: a
// room with a Running run keeps its activity series in any phase (S1 review I-3).
func roomObserver(m *metrics.Set, runs roomRuns) func(string, v1alpha1.RoomStatus, time.Time) {
	return func(room string, s v1alpha1.RoomStatus, last time.Time) {
		running := false
		for _, r := range runs.InRoom(room) {
			running = running || r.Phase == "Running"
		}
		m.ObserveRoom(room, s.Phase, running, int(s.PendingApprovals), last)
	}
}

// managerOptions watches Rooms in the broker's namespace and AgentRuns in theirs
// only: the RBAC (Task 1.18) grants exactly that, so a cluster-wide informer
// would be refused. Unstructured reads stay uncached, so the finalizer's run
// list and deletes see the API server, not the watch.
func managerOptions(ns string) ctrl.Options {
	runObj := &unstructured.Unstructured{}
	runObj.SetGroupVersionKind(runwatch.GVK())
	drain := managerDrain
	return ctrl.Options{
		LeaderElection: true, LeaderElectionID: "room-broker", LeaderElectionNamespace: ns,
		LeaderElectionReleaseOnCancel: true, GracefulShutdownTimeout: &drain,
		// :9090 serves the rooms_* set and the probes itself.
		Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0",
		Cache: cache.Options{ByObject: map[client.Object]cache.ByObject{
			&v1alpha1.Room{}: {Namespaces: map[string]cache.Config{ns: {}}},
			runObj:           {Namespaces: map[string]cache.Config{runwatch.Namespace: {}}},
		}},
		Client: client.Options{Cache: &client.CacheOptions{Unstructured: false}},
	}
}

// newManager builds the manager on managerOptions, logging through log.
func newManager(log *slog.Logger, ns string) (ctrl.Manager, error) {
	lr := logr.FromSlogHandler(log.Handler())
	// controller-runtime and client-go's leader election log through these globals.
	ctrl.SetLogger(lr)
	klog.SetLogger(lr)
	restCfg, err := ctrl.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("kubeconfig: %w", err)
	}
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, err
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	opts := managerOptions(ns)
	opts.Scheme, opts.Logger = scheme, lr
	mgr, err := ctrl.NewManager(restCfg, opts)
	if err != nil {
		return nil, fmt.Errorf("manager: %w", err)
	}
	return mgr, nil
}

func shutdownMetrics(ctx context.Context, log *slog.Logger, exp *metrics.Exporter) {
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), metricsDrain)
	defer cancel()
	if err := exp.Shutdown(dctx); err != nil {
		log.Warn("metrics shutdown", "err", err)
	}
}

// serveHTTP serves srv on ln until ctx ends, then drains for at most drain.
func serveHTTP(ctx context.Context, srv *http.Server, ln net.Listener, drain time.Duration) error {
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	select {
	case err := <-served:
		return fmt.Errorf("serve %s: %w", ln.Addr(), err)
	case <-ctx.Done():
	}
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), drain)
	defer cancel()
	shut := srv.Shutdown(dctx)
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		shut = errors.Join(shut, err)
	}
	if shut != nil {
		return fmt.Errorf("shut down %s: %w", ln.Addr(), shut)
	}
	return nil
}
