// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"golang.org/x/sync/errgroup"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	factoryv1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/factory/api"
	"github.com/Smana/agent-platform/internal/factory/config"
	"github.com/Smana/agent-platform/internal/factory/fmetrics"
	"github.com/Smana/agent-platform/internal/factory/forge"
	"github.com/Smana/agent-platform/internal/factory/intake"
	"github.com/Smana/agent-platform/internal/factory/killswitch"
	"github.com/Smana/agent-platform/internal/factory/meter"
	"github.com/Smana/agent-platform/internal/factory/reconciler"
	"github.com/Smana/agent-platform/internal/factory/rooms"
	"github.com/Smana/agent-platform/internal/factory/runs"
	"github.com/Smana/agent-platform/internal/factory/taskid"
	"github.com/Smana/agent-platform/internal/factory/tracing"
	"github.com/Smana/agent-platform/internal/factory/triage"
	"github.com/Smana/agent-platform/internal/httpx"
	"github.com/Smana/agent-platform/internal/metrics"
	"github.com/Smana/agent-platform/internal/policy"
	"github.com/Smana/agent-platform/internal/version"
)

// The factory's outbound bounds and GitHub's API root.
const (
	githubAPI     = "https://api.github.com/"
	githubTimeout = 15 * time.Second
	brokerTimeout = 15 * time.Second
	vmTimeout     = 10 * time.Second
	// pingEvery keeps "App token fresh" (forge.freshFor, 3 min) true on every healthy replica.
	pingEvery = time.Minute
	// traceDrain bounds the span exporter's flush at exit, inside the pod's grace.
	traceDrain = 2 * time.Second
)

// RunFactory runs agent-factory (SP3): the Task reconciler, the issue poller, the run meter
// and the stop's sweep on the leader; the run-request API on :8443, :9090 metrics and probes
// and the GitHub ping on every replica. getenv reads FACTORY_CONFIG and POD_NAMESPACE. A
// config that does not load fails the start, so a bad config fails the rollout rather than
// the running factory (§4).
func RunFactory(ctx context.Context, log *slog.Logger, getenv func(string) string) error {
	if err := runFactory(ctx, log, getenv); err != nil {
		return fmt.Errorf("agent-factory: %w", err)
	}
	return nil
}

func factoryEnv(getenv func(string) string) (cfgPath, ns string, err error) {
	var missing []error
	for _, kv := range []struct {
		name string
		dst  *string
	}{{"FACTORY_CONFIG", &cfgPath}, {"POD_NAMESPACE", &ns}} {
		if *kv.dst = getenv(kv.name); *kv.dst == "" {
			missing = append(missing, fmt.Errorf("%s is not set", kv.name))
		}
	}
	return cfgPath, ns, errors.Join(missing...)
}

func runFactory(ctx context.Context, log *slog.Logger, getenv func(string) string) error {
	cfgPath, ns, err := factoryEnv(getenv)
	if err != nil {
		return err
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	exp, err := metrics.NewExporter(metrics.FactoryBuildInfo, version.Version)
	if err != nil {
		return err
	}
	defer shutdownMetrics(ctx, log, exp)
	roots, err := rooms.LoadCA(cfg.Broker.CAFile)
	if err != nil {
		return err
	}
	broker, err := rooms.New(cfg.Broker.URL, cfg.Broker.TokenFile, httpx.New(brokerTimeout, roots), nil)
	if err != nil {
		return err
	}
	vm, err := meter.NewVM(cfg.Meter.URL, cfg.Meter.Query, httpx.New(vmTimeout, nil))
	if err != nil {
		return err
	}
	gh, err := forge.Connect(ctx, forge.Options{Repository: cfg.Repository, AppIDFile: cfg.GitHub.AppIDFile,
		PrivateKeyFile: cfg.GitHub.PrivateKeyFile, APIURL: githubAPI, UserAgent: "agent-factory/" + version.Version,
		HTTP: httpx.New(githubTimeout, nil), Now: time.Now})
	if err != nil {
		return err
	}

	sink, shutdownTrace, err := taskSink(ctx, cfg.Tracing.OTLPEndpoint)
	if err != nil {
		return err
	}
	defer shutdownTrace(context.WithoutCancel(ctx))

	mgr, err := newFactoryManager(log, ns)
	if err != nil {
		return err
	}
	m, err := fmetrics.New(exp.Meter(), mgr.GetClient(), ns, elected(mgr))
	if err != nil {
		return err
	}
	rc := runs.Client{C: mgr.GetClient()}
	rec := &reconciler.Reconciler{Client: mgr.GetClient(), Namespace: ns, Cfg: cfg, Forge: gh, Runs: rc, Rooms: broker,
		Triage:  triage.Classify{Cfg: cfg, C: triage.HTTPClassifier{URL: cfg.Triage.ClassifierURL}, Forge: gh},
		Metrics: m, Now: time.Now, Nonce: taskid.Random, Log: log,
		Trace: sink}
	if err := rec.SetupWithManager(mgr); err != nil {
		return err
	}
	// stopped is §6.1's pause: the stop object stops intake, and from phase 5 the API is intake
	// too — it refuses 503 kill_switch exactly when the poller stands down.
	stopped := func(ctx context.Context) bool {
		on, err := killswitch.Engaged(ctx, mgr.GetClient(), ns)
		return on || err != nil // intake pauses on doubt
	}
	// The day ledgers are invisible to the manager's cache: its ConfigMap informer carries a
	// metadata.name selector for the stop object (factoryManagerOptions). R50's reservation and
	// the meter's settle read and write date-named ledgers, so they go through a live client.
	live, err := client.New(mgr.GetConfig(), client.Options{Scheme: mgr.GetScheme()})
	if err != nil {
		return err
	}
	humansV, err := authn.NewVerifier(ctx, cfg.API.HumanIssuer, cfg.API.HumanJWKS)
	if err != nil {
		return err
	}
	var systemsV *authn.Verifier
	if cfg.API.SystemIssuer != "" {
		if systemsV, err = authn.NewVerifier(ctx, cfg.API.SystemIssuer, cfg.API.SystemJWKS); err != nil {
			return err
		}
	}
	clientIDs := func() []string { // re-read: the ExternalSecret may rotate a rebuilt client
		out := make([]string, 0, len(cfg.API.ClientIDFiles))
		for _, f := range cfg.API.ClientIDFiles {
			b, _ := os.ReadFile(filepath.Clean(f))
			out = append(out, strings.TrimSpace(string(b)))
		}
		return out
	}
	// The two groups R23 admits. The names are platform-fixed — the broker's config carries the
	// same values — and the factory's config has no groups block to name them from.
	groups := policy.Groups{Admin: "agents-admin", Member: "agents-member"}
	apiSrv := &api.Server{Auth: &api.Authenticator{Humans: humansV, Groups: groups, ClientIDs: clientIDs,
		Systems: systemsV, SystemAllow: cfg.API.SystemPrincipals},
		Cfg: cfg, Groups: groups, Runs: rc, Rooms: live, Namespace: ns, Stopped: stopped,
		NewRunID: taskid.Random, Now: time.Now, Metrics: m}
	for _, r := range []manager.Runnable{
		&intake.IssuePoller{Forge: gh, Client: mgr.GetClient(), Namespace: ns, Cfg: cfg, Errors: m, Log: log,
			Stopped: stopped},
		&meter.Meter{Runs: rc, Source: vm, Every: cfg.Poll.Meter.Duration, OnRevoke: m.Revoked, Log: log,
			Throttle: meter.VL{URL: cfg.Meter.LogsURL, Query: cfg.Meter.ThrottleQuery, HC: httpx.New(vmTimeout, nil)},
			Budgets:  cfg.Budgets, B1Ceiling: config.RunTokenCeiling, Now: time.Now,
			Remaining: func(p string, n, _ int64) { m.BudgetRemaining(ctx, p, n) },
			Ledger:    live, Namespace: ns},
		&killswitch.Sweeper{Reader: mgr.GetClient(), Namespace: ns, Runs: rc, Every: 15 * time.Second,
			OnSweep: func(n int) {
				for range n {
					m.Revoked(ctx, "manual")
				}
			}, Log: log},
		apiSrv,
		&pinger{ping: gh.Ping, every: pingEvery},
	} {
		if err := mgr.Add(r); err != nil {
			return fmt.Errorf("manager: %w", err)
		}
	}
	ops := factoryOps(gh.Healthy, time.Now, mgr.GetCache().WaitForCacheSync, func() bool { return ctx.Err() != nil }, exp.Handler())
	return serveFactory(ctx, log, mgr, ops, ns)
}

// serveFactory runs the manager until ctx ends or it fails; :9090 stops last, so the drain's
// final counts are scraped and /readyz answers 503 meanwhile, as the broker's does.
func serveFactory(ctx context.Context, log *slog.Logger, mgr manager.Manager, ops http.Handler, ns string) error {
	var lc net.ListenConfig
	opsLn, err := lc.Listen(ctx, "tcp", opsAddr)
	if err != nil {
		return fmt.Errorf("ops listener: %w", err)
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
	g, gctx := errgroup.WithContext(runCtx)
	g.Go(func() error {
		if err := mgr.Start(gctx); err != nil {
			return fmt.Errorf("manager: %w", err)
		}
		return nil
	})
	log.Info("agent-factory starting", "version", version.Version, "namespace", ns)
	err = g.Wait()
	stopOps()
	return errors.Join(err, <-opsDone)
}

// taskSink exports task spans (R46) when tracing.otlpEndpoint is set, and is a nil interface
// otherwise, never a typed nil the reconciler would call. The exporter dials lazily: an
// unreachable collector loses spans, never the factory's start.
func taskSink(ctx context.Context, endpoint string) (tracing.Sink, func(context.Context), error) {
	if endpoint == "" {
		return nil, func(context.Context) {}, nil
	}
	exp, err := tracing.NewOTLP(ctx, endpoint)
	if err != nil {
		return nil, nil, err
	}
	return exp, func(ctx context.Context) {
		dctx, cancel := context.WithTimeout(ctx, traceDrain)
		defer cancel()
		_ = exp.Shutdown(dctx)
	}, nil
}

// factoryOps is :9090 for the factory: /readyz once the caches synced and the App token is fresh
// (§6.5); /startupz at once, since the server starts only after the config parsed.
func factoryOps(fresh func(time.Time) bool, now func() time.Time, synced func(context.Context) bool,
	draining func() bool, metricsH http.Handler,
) http.Handler {
	ping := func(context.Context) error {
		if !fresh(now()) {
			return errors.New("GitHub App token not fresh")
		}
		return nil
	}
	started := func(context.Context) (bool, error) { return true, nil }
	return opsHandler(ping, started, boundedSync(synced), draining, metricsH)
}

// elected reports whether this replica leads; the leader-only gauges read it.
func elected(mgr manager.Manager) func() bool {
	return func() bool {
		select {
		case <-mgr.Elected():
			return true
		default:
			return false
		}
	}
}

// factoryManagerOptions caches Tasks, Rooms and the stop object in the factory's namespace and
// AgentRuns in theirs only: the chart's Roles grant exactly that. Unstructured reads stay
// uncached, so a stop's run list sees the API server (ruling SN), not the watch.
func factoryManagerOptions(ns string) ctrl.Options {
	runObj := &unstructured.Unstructured{}
	runObj.SetGroupVersionKind(runs.GVK())
	drain := managerDrain
	here := map[string]cache.Config{ns: {}}
	return ctrl.Options{
		LeaderElection: true, LeaderElectionID: "agent-factory.agents.ogenki.io", LeaderElectionNamespace: ns,
		LeaderElectionReleaseOnCancel: true, GracefulShutdownTimeout: &drain,
		// :9090 serves the agent_factory_* set and the probes itself.
		Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0",
		Cache: cache.Options{ByObject: map[client.Object]cache.ByObject{
			&factoryv1.Task{}: {Namespaces: here},
			&v1alpha1.Room{}:  {Namespaces: here},
			&corev1.ConfigMap{}: {Namespaces: here,
				Field: fields.OneTermEqualSelector("metadata.name", killswitch.ConfigMap)},
			runObj: {Namespaces: map[string]cache.Config{runs.Namespace: {}}},
		}},
		Client: client.Options{Cache: &client.CacheOptions{Unstructured: false}},
	}
}

// newFactoryManager builds the manager on factoryManagerOptions, logging through log.
func newFactoryManager(log *slog.Logger, ns string) (ctrl.Manager, error) {
	lr := logr.FromSlogHandler(log.Handler())
	// controller-runtime and client-go's leader election log through these globals.
	ctrl.SetLogger(lr)
	klog.SetLogger(lr)
	restCfg, err := ctrl.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("kubeconfig: %w", err)
	}
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{clientgoscheme.AddToScheme, factoryv1.AddToScheme, v1alpha1.AddToScheme} {
		if err := add(scheme); err != nil {
			return nil, err
		}
	}
	runs.Scheme(scheme)
	opts := factoryManagerOptions(ns)
	opts.Scheme, opts.Logger = scheme, lr
	mgr, err := ctrl.NewManager(restCfg, opts)
	if err != nil {
		return nil, fmt.Errorf("manager: %w", err)
	}
	return mgr, nil
}

// pinger keeps /readyz honest on every replica, leader or not: a follower makes no other call to
// GitHub, and "App token fresh" is a recent success. A failed ping is only a stale token.
type pinger struct {
	ping  func(context.Context) error
	every time.Duration
	// ticker paces the pings; nil means a time.Ticker.
	ticker func(d time.Duration) (c <-chan time.Time, stop func())
}

// NeedLeaderElection is false: every replica answers its own readiness.
func (*pinger) NeedLeaderElection() bool { return false }

// Start pings at once, then every period, until ctx ends.
func (p *pinger) Start(ctx context.Context) error {
	tick, stop := p.tick()
	defer stop()
	for {
		_ = p.ping(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-tick:
		}
	}
}

func (p *pinger) tick() (<-chan time.Time, func()) {
	if p.ticker != nil {
		return p.ticker(p.every)
	}
	t := time.NewTicker(p.every)
	return t.C, t.Stop
}
