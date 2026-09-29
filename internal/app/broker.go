// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"sync/atomic"
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
	"github.com/Smana/agent-platform/internal/metrics"
	"github.com/Smana/agent-platform/internal/redact"
	"github.com/Smana/agent-platform/internal/roomctrl"
	"github.com/Smana/agent-platform/internal/runwatch"
	"github.com/Smana/agent-platform/internal/store"
	"github.com/Smana/agent-platform/internal/version"
)

// The broker's listeners and bounds (§9 ports; GP-18 for :8443).
const (
	bridgeAddr = ":8443"
	opsAddr    = ":9090"
	// observeTimeout bounds one run's lifecycle append from the informer's
	// handler: a stalled database must not freeze the liveness mirror, which
	// every replica's bridge admission reads.
	observeTimeout = 10 * time.Second
	// sweepEvery and sweepTimeout pace the leader's "joined, never left" sweep.
	sweepEvery   = 5 * time.Minute
	sweepTimeout = time.Minute
	// The drains fit the pod's 30 s grace: the manager and :8443 in parallel,
	// then the metrics provider.
	bridgeDrain  = 20 * time.Second
	managerDrain = 20 * time.Second
	opsDrain     = 2 * time.Second
	metricsDrain = time.Second
)

// RunBroker runs room-broker's subcommand args[0]: serve (the default) or
// retention. getenv reads ROOMS_CONFIG, ROOMS_DATABASE_URL and POD_NAMESPACE.
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

// serveBroker runs the Room controller, the AgentRun watch, the :8443 API and
// the :9090 metrics and probes until ctx ends or one of them fails.
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
	exp, err := metrics.NewExporter(version.Version)
	if err != nil {
		return fmt.Errorf("room-broker: %w", err)
	}
	defer shutdownMetrics(ctx, log, exp)
	m, err := metrics.New(exp.Meter(), nil)
	if err != nil {
		return fmt.Errorf("room-broker: %w", err)
	}
	st, err := store.Open(ctx, dsn)
	if err != nil {
		return fmt.Errorf("room-broker: %w", err)
	}
	defer st.Close()
	logStore := &meteredLog{Store: st, appends: st, m: m, now: time.Now}
	red, err := redact.New()
	if err != nil {
		return fmt.Errorf("room-broker: redaction rules: %w", err)
	}
	runs, systems, err := authenticators(ctx, log, cfg, m)
	if err != nil {
		return fmt.Errorf("room-broker: %w", err)
	}

	mgr, err := newManager(log, ns)
	if err != nil {
		return fmt.Errorf("room-broker: %w", err)
	}
	watch := runwatch.New()
	if err := runwatch.Register(ctx, mgr.GetCache(), watch); err != nil {
		return fmt.Errorf("room-broker: %w", err)
	}
	// Every replica mirrors the runs (bridge admission); only the leader appends
	// their lifecycle to the log.
	var leading atomic.Bool
	events := &runwatch.Events{Store: logStore}
	watch.OnChange(bounded(&leading, log, "record a run's lifecycle", events.Observe))
	// A claim deleted mid-run never reaches a terminal phase in the watch (review M15).
	watch.OnRemove(bounded(&leading, log, "record a deleted run", events.ObserveDeleted))
	if err := mgr.Add(&leader{active: &leading, synced: mgr.GetCache().WaitForCacheSync,
		replay: func(ctx context.Context) {
			for _, r := range watch.All() {
				bounded(&leading, log, "replay a run's lifecycle", events.Observe)(ctx, r)
			}
		},
		sweep: func(ctx context.Context) {
			ctx, cancel := context.WithTimeout(ctx, sweepTimeout)
			defer cancel()
			if err := events.Sweep(ctx, st, watch.Get); err != nil {
				log.Error("sweep the runs that joined and never left", "err", err)
			}
		},
		every: sweepEvery}); err != nil {
		return fmt.Errorf("room-broker: leader: %w", err)
	}
	rc := &roomctrl.Reconciler{Client: mgr.GetClient(), APIReader: mgr.GetAPIReader(), Store: logStore,
		Runs: watch, Ends: events, Log: log,
		Observe: func(room string, s v1alpha1.RoomStatus, last time.Time) {
			m.ObserveRoom(room, s.Phase, int(s.PendingApprovals), last)
		}}
	if err := rc.SetupWithManager(mgr); err != nil {
		return fmt.Errorf("room-broker: %w", err)
	}

	api := &bridgeapi.Server{Log: logStore, Redactor: red, Runs: runs, Systems: systems, Watch: watch, Logger: log}
	watch.OnGone(api.Drop)
	ops := opsHandler(st.Ping, st.SchemaReady, cacheSynced(mgr.GetCache()), exp.Handler())

	var lc net.ListenConfig
	opsLn, err := lc.Listen(ctx, "tcp", opsAddr)
	if err != nil {
		return fmt.Errorf("room-broker: ops listener: %w", err)
	}
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return serveHTTP(gctx, opsServer(ops, log), opsLn, opsDrain) })
	g.Go(func() error { return api.ListenAndServeTLS(gctx, bridgeAddr, tlsCfg, bridgeDrain) })
	g.Go(func() error {
		if err := mgr.Start(gctx); err != nil {
			return fmt.Errorf("manager: %w", err)
		}
		return nil
	})
	log.Info("room-broker starting", "version", version.Version, "namespace", ns)
	if err := g.Wait(); err != nil {
		return fmt.Errorf("room-broker: %w", err)
	}
	return nil
}

// authenticators builds the run and system verifiers; each fetches its JWKS now,
// so an unreachable issuer fails the rollout.
func authenticators(ctx context.Context, log *slog.Logger, cfg config.Config, m *metrics.Set) (*authn.Runs, *authn.Systems, error) {
	lastRefresh := map[string]func() time.Time{}
	var issuers []authn.RunIssuer
	for _, is := range cfg.RunIssuers {
		v, err := authn.NewVerifier(ctx, is.Issuer, is.JWKSURL, authn.WithLogger(log))
		if err != nil {
			return nil, nil, err
		}
		lastRefresh[is.Issuer] = v.LastRefresh
		// config.Load compiled it with one capture group already.
		issuers = append(issuers, authn.RunIssuer{Verifier: v, SubPattern: regexp.MustCompile(is.SubPattern)})
	}
	runs, err := authn.NewRuns(issuers...)
	if err != nil {
		return nil, nil, err
	}
	sys, err := authn.NewVerifier(ctx, cfg.SystemIssuer.Issuer, cfg.SystemIssuer.JWKSURL, authn.WithLogger(log))
	if err != nil {
		return nil, nil, err
	}
	// One issuer can sign both kinds of token: its series is the freshest fetch.
	if runLast, ok := lastRefresh[cfg.SystemIssuer.Issuer]; ok {
		lastRefresh[cfg.SystemIssuer.Issuer] = func() time.Time { return later(runLast(), sys.LastRefresh()) }
	} else {
		lastRefresh[cfg.SystemIssuer.Issuer] = sys.LastRefresh
	}
	if err := m.WatchJWKS(lastRefresh); err != nil {
		return nil, nil, err
	}
	return runs, authn.NewSystems(sys, cfg.SystemPrincipals), nil
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// newManager watches Rooms in the broker's namespace and AgentRuns in theirs
// only: the RBAC (Task 1.18) grants exactly that, so a cluster-wide informer
// would be refused. Unstructured reads stay uncached (the default), so the
// finalizer's run list and deletes see the API server, not the watch.
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
	runObj := &unstructured.Unstructured{}
	runObj.SetGroupVersionKind(runwatch.GVK())
	drain := managerDrain
	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme: scheme, Logger: lr,
		LeaderElection: true, LeaderElectionID: "room-broker", LeaderElectionNamespace: ns,
		LeaderElectionReleaseOnCancel: true, GracefulShutdownTimeout: &drain,
		// :9090 serves the rooms_* set and the probes itself.
		Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0",
		Cache: cache.Options{ByObject: map[client.Object]cache.ByObject{
			&v1alpha1.Room{}: {Namespaces: map[string]cache.Config{ns: {}}},
			runObj:           {Namespaces: map[string]cache.Config{runwatch.Namespace: {}}},
		}},
		Client: client.Options{Cache: &client.CacheOptions{Unstructured: false}},
	})
	if err != nil {
		return nil, fmt.Errorf("manager: %w", err)
	}
	return mgr, nil
}

// bounded runs a lifecycle append for the leader only, bounded by observeTimeout,
// and logs its failure: the watch's callbacks return nothing, and the sweep
// retries what failed.
func bounded(leading *atomic.Bool, log *slog.Logger, msg string, f func(context.Context, runwatch.Run) error) func(context.Context, runwatch.Run) {
	return func(ctx context.Context, r runwatch.Run) {
		if !leading.Load() {
			return
		}
		ctx, cancel := context.WithTimeout(ctx, observeTimeout)
		defer cancel()
		if err := f(ctx, r); err != nil {
			log.Error(msg, "run", r.ID, "room", r.Room, "err", err)
		}
	}
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
