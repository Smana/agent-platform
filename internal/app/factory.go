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
	"go.opentelemetry.io/otel/trace"
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
	"github.com/Smana/agent-platform/internal/factory/fence"
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
	"github.com/Smana/agent-platform/internal/runwatch"
	"github.com/Smana/agent-platform/internal/version"
)

// The factory's outbound bounds and GitHub's API root.
const (
	githubAPI     = "https://api.github.com/"
	githubTimeout = 15 * time.Second
	brokerTimeout = 15 * time.Second
	vmTimeout     = 10 * time.Second
	// C7's own deadline is 2 s.
	classifierTimeout = 3 * time.Second
	// pingEvery keeps the installation token warm and forge.Healthy true on every replica.
	pingEvery = time.Minute
	// traceDrain bounds the span exporter's flush at exit, inside the pod's grace.
	traceDrain = 2 * time.Second
	// fenceEvery is how often each repository's fence file is read (v2 D7); fenceStale how long a
	// failed read keeps the last answer before the repository counts as not owned.
	fenceEvery = 5 * time.Minute
	fenceStale = 15 * time.Minute
)

// RunFactory runs agent-factory (SP3): the Task reconciler, the issue poller, the run meter
// and the stop's sweep on the leader; the run-request API on :8082 (plaintext inside the mesh; the broker's :8443 is the TLS listener), :9090 metrics and probes
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

// warnIgnored names each key the config sets that this release accepts and ignores: a file written
// for the previous release loads, and its owner learns which key to move to (#155).
func warnIgnored(log *slog.Logger, cfg *config.Config) {
	for _, k := range cfg.Ignored {
		log.Warn("config key ignored: the daily budgets are in USD", "key", k.Key, "use", k.Use)
	}
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
	warnIgnored(log, cfg)
	// Before anything watches or creates a run: every reader takes the kind from here.
	if err := runwatch.Configure(cfg.Runs); err != nil {
		return fmt.Errorf("factory config: runs: %w", err)
	}
	exp, err := metrics.NewExporter(metrics.FactoryBuildInfo, version.Version)
	if err != nil {
		return err
	}
	defer shutdownMetrics(ctx, log, exp)
	sink, tp, shutdownTrace, err := taskSink(ctx, cfg.Tracing.OTLPEndpoint)
	if err != nil {
		return err
	}
	defer shutdownTrace(context.WithoutCancel(ctx))

	roots, err := rooms.LoadCA(cfg.Broker.CAFile)
	if err != nil {
		return err
	}
	broker, err := rooms.New(cfg.Broker.URL, cfg.Broker.TokenFile, httpx.New(brokerTimeout, roots), tp)
	if err != nil {
		return err
	}
	vm, err := meter.NewVM(cfg.Meter.URL, cfg.Meter.Query, httpx.New(vmTimeout, nil))
	if err != nil {
		return err
	}
	conns, err := connectRepositories(ctx, log, cfg)
	if err != nil {
		return err
	}
	mgr, err := newFactoryManager(log, ns)
	if err != nil {
		return err
	}
	m, err := fmetrics.New(exp.Meter(), mgr.GetClient(), ns, elected(mgr))
	if err != nil {
		return err
	}
	m.ListRepositories(conns.names()...)
	m.ChargePeople(cfg.Budgets.PerPerson.Enabled)
	sponsors := map[string]string{}
	for _, sp := range cfg.API.Sponsored {
		sponsors[sp.Principal()] = factoryv1.Person(sp.Sponsor)
	}
	m.Sponsors(sponsors)
	// v2 D7: a repository is acted on only while its default branch names this factory.
	fenced := &fence.Fence{Name: cfg.Factory, Repositories: conns.names(), Read: conns.fenceFile,
		Every: fenceEvery, Stale: fenceStale, Now: time.Now, OnState: m.RepositoryOwned, Log: log}
	costs := &meter.Costs{} // the meter writes each run's cost, the reconciler sums a task's
	runUSD := map[string]float64{}
	for name, t := range cfg.Tiers {
		if t.RunUSD > 0 {
			runUSD[name] = t.RunUSD
			m.RunBudgetUSD(ctx, name, t.RunUSD)
		}
	}
	// The day ledgers are invisible to the manager's cache: its ConfigMap informer carries a
	// metadata.name selector for the stop object (factoryManagerOptions). R50's reservation, the
	// meter's settle and Queued's daily cap (which reserves its runs there, #78) read and write
	// date-named ledgers, so through a live client.
	live, err := client.New(mgr.GetConfig(), client.Options{Scheme: mgr.GetScheme()})
	if err != nil {
		return err
	}
	rc := runs.Client{C: mgr.GetClient()}
	perms := conns.permissions(cfg.API.Access)
	rec := &reconciler.Reconciler{Client: mgr.GetClient(), Ledger: live, Namespace: ns, Cfg: cfg, Clients: conns.task, Owned: fenced.Owned,
		CanWrite: perms.CanWrite, Runs: rc, Rooms: broker,
		Triage: triage.Classify{Cfg: cfg, C: triage.HTTPClassifier{URL: cfg.Triage.ClassifierURL, HC: httpx.New(classifierTimeout, nil)},
			Clients: conns.class},
		Metrics: m, Now: time.Now, Nonce: taskid.Random, Log: log,
		Trace: sink, Costs: costs}
	if err := rec.SetupWithManager(mgr); err != nil {
		return err
	}
	// stopped is §6.1's pause: the stop object stops intake, and from phase 5 the API is intake
	// too — it refuses 503 kill_switch exactly when the poller stands down.
	stopped := func(ctx context.Context) bool {
		on, err := killswitch.Engaged(ctx, mgr.GetClient(), ns)
		return on || err != nil // intake pauses on doubt
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
	// The two groups R23 admits: api.groups, which defaults to the values the broker's config carries.
	groups := policy.Groups{Admin: cfg.API.Groups.Admin, Member: cfg.API.Groups.Member}
	access := conns.access(log, cfg, perms)
	people, _ := access.(api.PersonResolver) // D1: the login the write check reads is the person charged
	apiSrv := &api.Server{Auth: &api.Authenticator{Humans: humansV, Groups: groups, ClientIDs: clientIDs,
		Systems: systemsV, SystemAllow: cfg.API.SystemPrincipals},
		Cfg: cfg, Groups: groups, Runs: rc, Rooms: live, Namespace: ns, Stopped: stopped,
		NewRunID: taskid.Random, Now: time.Now, Metrics: m, Access: access, People: people, Owned: fenced.Owned}
	runlore, ok := conns.get(cfg.RunLoreRepository())
	if !ok {
		return fmt.Errorf("runlore.repository %s has no GitHub connection", cfg.RunLoreRepository())
	}
	var runnables []manager.Runnable
	for _, rp := range cfg.Repos() {
		c, _ := conns.get(rp.Name)
		name := rp.Name
		runnables = append(runnables, &intake.IssuePoller{Forge: c.gh, Client: mgr.GetClient(), Namespace: ns, Cfg: cfg,
			Repository: name, Errors: m, Log: log.With("repository", name), Stopped: stopped, Owned: fenced.Owned, Ledger: live,
			CanWrite: func(ctx context.Context, login string) (bool, error) { return perms.CanWrite(ctx, name, login) }})
	}
	for _, r := range append(runnables,
		&intake.Scheduler{Clients: conns.schedule, Owned: fenced.Owned, Client: mgr.GetClient(), Namespace: ns, Cfg: cfg,
			Stopped: stopped, Now: time.Now, Errors: m, Log: log},
		// FA-8: RunLore pushes findings over the tailnet; the token file is re-read per request,
		// so the ExternalSecret's rotations need no restart (AGENTS.md).
		&intake.RunLore{Forge: runlore.gh, Owned: fenced.Owned, Client: mgr.GetClient(), Namespace: ns, Cfg: cfg,
			Token: func() string {
				b, _ := os.ReadFile(filepath.Clean(cfg.RunLore.TokenFile))
				return strings.TrimSpace(string(b))
			},
			Stopped: stopped, Now: time.Now, Errors: m, Log: log},
		&meter.Meter{Runs: rc, Source: vm, Every: cfg.Poll.Meter.Duration, OnRevoke: m.RunRevoked, Log: log,
			OnCharge: m.Charged, OnCharges: m.RunCharges,
			Prices: cfg.Meter.Prices, RunUSD: runUSD, OnCost: m.RunCost, OnLateCost: m.LateCost, OnSpend: m.RunSpend, Costs: costs,
			OnFallback: func(ctx context.Context, why string) { m.RunBudgetFallback(ctx, "run", why) },
			OnUnpriced: m.ModelUnpriced, OnInputExcludesCache: m.InputExcludesCache,
			Throttle: meter.VL{URL: cfg.Meter.LogsURL, Query: cfg.Meter.ThrottleQuery, HC: httpx.New(vmTimeout, nil)},
			Budgets:  cfg.Budgets, B1Ceiling: config.RunTokenCeiling, Now: time.Now,
			Day:          func(p string, spent, c int64) { m.BudgetDay(ctx, p, spent, c) },
			OnUsageError: m.MeterUsageReadError, OnSucceeded: m.MeterSucceeded, OnOvershoot: m.RunOvershoot,
			Ledger: live, Tasks: mgr.GetClient(), Namespace: ns},
		&killswitch.Sweeper{Reader: mgr.GetClient(), Namespace: ns, Runs: rc, Every: 15 * time.Second,
			Release: func(ctx context.Context, r runs.Run) error {
				return api.Release(ctx, live, ns, time.Now(), r.ID) // admission reads today's ledger only
			},
			OnSweep: func(revoked []runs.Run) {
				for _, r := range revoked {
					m.Revoked(fmetrics.WithPrincipal(ctx, r.Payer()), "manual")
				}
			}, Log: log},
		apiSrv, fenced,
		&pinger{ping: conns.ping, every: pingEvery, up: func() bool { return conns.healthy(time.Now()) }, record: m.GitHubUp},
	) {
		if err := mgr.Add(r); err != nil {
			return fmt.Errorf("manager: %w", err)
		}
	}
	ops := factoryOps(mgr.GetCache().WaitForCacheSync, func() bool { return ctx.Err() != nil }, exp.Handler())
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

// connectMerger connects the merger App, or answers the fail-closed forge.Absent when its key
// is not mounted and no class needs it (#57: the chart mounts the Secret optional). A class
// live or in shadow reads checks through the merger, so for it a missing key stops the start.
func connectMerger(ctx context.Context, log *slog.Logger, cfg *config.Config, repository string) (forge.Merger, error) {
	idFile := cfg.GitHub.MergerAppIDFile
	if idFile != "" {
		_, err := os.Stat(filepath.Clean(idFile))
		switch {
		case err == nil:
		case errors.Is(err, os.ErrNotExist) && !cfg.NeedsMerger():
			idFile = ""
		case errors.Is(err, os.ErrNotExist):
			return nil, fmt.Errorf("merger App key %s is missing, and a live or shadow class needs it: create the agent-factory-merger Secret or turn the class off", idFile)
		}
	}
	if idFile == "" {
		log.Warn("no merger App key: checks, merges and reverts fail closed until it is mounted and the pod restarts")
		return forge.Absent{}, nil
	}
	return forge.Connect(ctx, forge.Options{Repository: repository, AppIDFile: cfg.GitHub.MergerAppIDFile,
		PrivateKeyFile: cfg.GitHub.MergerKeyFile, APIURL: githubAPI, UserAgent: "agent-factory-merger/" + version.Version,
		HTTP: httpx.New(githubTimeout, nil), Now: time.Now, Permissions: forge.MergerPermissions(cfg.AnyCheckReruns(), cfg.AnyLive())})
}

// taskSink exports task spans (R46) when tracing.otlpEndpoint is set, and is a nil interface
// otherwise, never a typed nil the reconciler would call. The exporter dials lazily: an
// unreachable collector loses spans, never the factory's start.
func taskSink(ctx context.Context, endpoint string) (tracing.Sink, trace.TracerProvider, func(context.Context), error) {
	if endpoint == "" {
		return nil, nil, func(context.Context) {}, nil
	}
	exp, err := tracing.NewOTLP(ctx, endpoint)
	if err != nil {
		return nil, nil, nil, err
	}
	return exp, exp.Provider(), func(ctx context.Context) {
		dctx, cancel := context.WithTimeout(ctx, traceDrain)
		defer cancel()
		_ = exp.Shutdown(dctx)
	}, nil
}

// factoryOps is :9090 for the factory: /readyz once the caches synced; /startupz at once, since
// the server starts only after the config parsed. Readiness does not look at GitHub (#58): an
// outage would unready every replica at once, and an unready replica serves nothing less than a
// ready one — the API and the intake need no GitHub call to answer, and the loops that do
// retry on their own. The outage shows in the loops' error logs and the intake error counter.
func factoryOps(synced func(context.Context) bool, draining func() bool, metricsH http.Handler) http.Handler {
	noDependency := func(context.Context) error { return nil }
	started := func(context.Context) (bool, error) { return true, nil }
	return opsHandler(noDependency, started, boundedSync(synced), draining, metricsH)
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
			runObj: {Namespaces: map[string]cache.Config{runs.Namespace(): {}}},
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

// pinger probes GitHub on every replica, leader or not: a follower makes no other call to it. A
// failed ping is not an error to act on. The pinger reports whether GitHub answered recently as
// agent_factory_github_up, the outage signal now that /readyz ignores GitHub (#58).
type pinger struct {
	ping  func(context.Context) error
	every time.Duration
	// up says whether a call succeeded recently; record publishes it. A nil record reports nothing.
	up     func() bool
	record func(ctx context.Context, up bool)
	// ticker paces the pings; nil means a time.Ticker.
	ticker func(d time.Duration) (c <-chan time.Time, stop func())
}

// NeedLeaderElection is false: every replica reports its own view of GitHub.
func (*pinger) NeedLeaderElection() bool { return false }

// Start pings at once, then every period, until ctx ends.
func (p *pinger) Start(ctx context.Context) error {
	tick, stop := p.tick()
	defer stop()
	for {
		_ = p.ping(ctx)
		if p.record != nil {
			p.record(ctx, p.up())
		}
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
