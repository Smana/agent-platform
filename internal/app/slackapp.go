// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Smana/agent-platform/internal/httpx"
	"github.com/Smana/agent-platform/internal/metrics"
	"github.com/Smana/agent-platform/internal/slackapp"
	"github.com/Smana/agent-platform/internal/version"
	"github.com/Smana/agent-platform/internal/webhook"
)

const (
	slackTimeout = 15 * time.Second
	slackDrain   = 10 * time.Second
)

// slackRunning is a started App: the three bound addresses and Wait, which returns once the
// context ended and the listeners drained, or the first one failed.
type slackRunning struct {
	Events, Webhooks, Ops net.Addr
	Wait                  func() error
}

// RunSlackApp runs factory-slack-app: Slack's events, the broker's webhooks and the ops
// listener, until ctx ends. args is `--config <file>` (SLACK_APP_CONFIG when absent).
func RunSlackApp(ctx context.Context, log *slog.Logger, args []string, getenv func(string) string) error {
	run, err := startSlackApp(ctx, log, args, getenv, inClusterClient)
	if err != nil {
		return fmt.Errorf("factory-slack-app: %w", err)
	}
	return run.Wait()
}

func inClusterClient() (client.Client, error) {
	cfg, err := ctrl.GetConfig()
	if err != nil {
		return nil, err
	}
	return client.New(cfg, client.Options{Scheme: clientgoscheme.Scheme})
}

// startSlackApp reads the config, wires the App and returns once the three listeners are bound.
// newClient builds the cluster client the thread map lives behind.
func startSlackApp(ctx context.Context, log *slog.Logger, args []string, getenv func(string) string,
	newClient func() (client.Client, error),
) (*slackRunning, error) {
	fs := flag.NewFlagSet("factory-slack-app", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	path := fs.String("config", getenv("SLACK_APP_CONFIG"), "the App's config file")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if *path == "" {
		return nil, errors.New("--config (or SLACK_APP_CONFIG) is required")
	}
	cfg, err := slackapp.LoadConfig(*path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if cfg.IssuerURL == "" || cfg.ClientIDFile == "" || cfg.ClientSecretFile == "" || cfg.ProjectIDFile == "" {
		return nil, errors.New("config: issuerURL, clientIDFile, clientSecretFile and projectIDFile are required")
	}
	if err := checkSlackInputs(cfg); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	exp, err := metrics.NewExporter(metrics.SlackBuildInfo, version.Version)
	if err != nil {
		return nil, err
	}
	rec, err := newSlackRecorder(exp)
	if err != nil {
		return nil, err
	}
	cl, err := newClient()
	if err != nil {
		return nil, fmt.Errorf("cluster client: %w", err)
	}
	a := newSlackApp(cfg, cl, rec, log)

	var lc net.ListenConfig
	lns := make([]net.Listener, 0, 3)
	for _, addr := range []string{cfg.EventsAddr, cfg.WebhooksAddr, cfg.OpsAddr} {
		ln, err := lc.Listen(ctx, "tcp", addr)
		if err != nil {
			for _, l := range lns {
				_ = l.Close()
			}
			return nil, fmt.Errorf("listen %s: %w", addr, err)
		}
		lns = append(lns, ln)
	}
	events, hooks, opsLn := lns[0], lns[1], lns[2]
	ops := opsHandler(func(context.Context) error { return nil }, func(context.Context) (bool, error) { return true, nil },
		func(context.Context) bool { return true }, func() bool { return ctx.Err() != nil }, exp.Handler())

	runCtx, cancelRun := context.WithCancel(ctx)
	opsCtx, stopOps := context.WithCancel(context.WithoutCancel(ctx))
	var (
		mu         sync.Mutex
		errs       []error
		front, bak sync.WaitGroup // the two public listeners, then ops
	)
	serve := func(wg *sync.WaitGroup, c context.Context, h http.Handler, ln net.Listener) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := serveHTTP(c, slackServer(h, log), ln, slackDrain); err != nil {
				mu.Lock()
				errs = append(errs, err)
				mu.Unlock()
				cancelRun() // a dead listener takes the rest down: the pod would be half-working
			}
		}()
	}
	serve(&front, runCtx, routed("POST /slack/events", a.Events()), events)
	serve(&front, runCtx, routed("POST /webhooks", a.Webhooks()), hooks)
	serve(&bak, opsCtx, ops, opsLn)
	log.Info("factory-slack-app starting", "version", version.Version,
		"events", events.Addr().String(), "webhooks", hooks.Addr().String(), "ops", opsLn.Addr().String())

	return &slackRunning{Events: events.Addr(), Webhooks: hooks.Addr(), Ops: opsLn.Addr(), Wait: func() error {
		front.Wait()
		stopOps() // :9090 stops last, so the drain's final counts are scraped and /readyz says 503 meanwhile
		bak.Wait()
		cancelRun()
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), opsDrain)
		defer cancel()
		_ = exp.Shutdown(dctx)
		mu.Lock()
		defer mu.Unlock()
		return errors.Join(errs...)
	}}, nil
}

// checkSlackInputs refuses to start on a file the App reads later and cannot: each secret file
// must be readable and non-empty, the webhook key must parse, the issuer must be an https URL
// (http only on loopback, for tests). It names the field, never the content.
func checkSlackInputs(cfg slackapp.Config) error {
	var errs []error
	for _, f := range []struct{ field, path string }{
		{"signingSecretFile", cfg.SigningSecretFile}, {"botTokenFile", cfg.BotTokenFile},
		{"webhookKeyFile", cfg.WebhookKeyFile}, {"clientIDFile", cfg.ClientIDFile},
		{"clientSecretFile", cfg.ClientSecretFile}, {"projectIDFile", cfg.ProjectIDFile},
		{"botUserIDFile", cfg.BotUserIDFile},
	} {
		if fileValue(f.path) == "" {
			errs = append(errs, fmt.Errorf("%s is empty or unreadable", f.field))
		}
	}
	if _, err := webhook.ReadKeys(cfg.WebhookKeyFile); err != nil && fileValue(cfg.WebhookKeyFile) != "" {
		errs = append(errs, errors.New("webhookKeyFile does not hold one or two base64 keys of at least 32 bytes"))
	}
	u, err := url.Parse(cfg.IssuerURL)
	switch {
	case err != nil || u.Hostname() == "" || !u.IsAbs():
		errs = append(errs, errors.New("issuerURL must be an absolute https URL"))
	case u.Scheme == "https":
	case u.Scheme == "http" && isLoopback(u.Hostname()):
	default:
		errs = append(errs, errors.New("issuerURL must be an https URL (http only on a loopback host)"))
	}
	return errors.Join(errs...)
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// routed serves h on exactly one route; anything else is a 404.
func routed(pattern string, h http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle(pattern, h)
	return mux
}

// slackServer bounds a listener (nothing here streams). WriteTimeout outlasts the webhook
// handler's 30s context, so a slow delivery answers 200 or 502 instead of a cut connection.
func slackServer(h http.Handler, log *slog.Logger) *http.Server {
	return &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 45 * time.Second, MaxHeaderBytes: 16 << 10,
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn)}
}

// fileValue reads a one-line secret file at use, so a rotated Secret applies; "" when unreadable.
func fileValue(path string) string {
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// newSlackApp wires the App from its config. TeamID must come from the config: an empty one
// drops every event.
func newSlackApp(cfg slackapp.Config, cl client.Client, rec slackapp.Recorder, log *slog.Logger) *slackapp.App {
	hc := httpx.New(slackTimeout, nil)
	tokens := &slackapp.Tokens{Fetch: func(ctx context.Context) (*oauth2.Token, error) {
		cc := clientcredentials.Config{ClientID: fileValue(cfg.ClientIDFile), ClientSecret: fileValue(cfg.ClientSecretFile),
			TokenURL: strings.TrimSuffix(cfg.IssuerURL, "/") + "/oauth/v2/token",
			Scopes:   []string{"openid", "urn:zitadel:iam:org:project:id:" + fileValue(cfg.ProjectIDFile) + ":aud"}}
		return cc.Token(context.WithValue(ctx, oauth2.HTTPClient, hc))
	}}
	return &slackapp.App{
		Slack:    &slackapp.SlackClient{Token: func() string { return fileValue(cfg.BotTokenFile) }, HC: hc, Logger: log},
		Broker:   slackapp.Broker{URL: cfg.BrokerURL, Tokens: tokens, HC: hc},
		Threads:  slackapp.ConfigMapThreads{C: cl, Namespace: cfg.Namespace, Now: time.Now, Logger: log},
		Verifier: slackapp.Verifier{Secret: func() string { return fileValue(cfg.SigningSecretFile) }, Now: time.Now},
		WebhookKeys: func() ([][]byte, error) {
			return webhook.ReadKeys(cfg.WebhookKeyFile)
		},
		TeamID: cfg.TeamID, BotUserID: fileValue(cfg.BotUserIDFile), RoomsURL: cfg.RoomsURL, Channels: cfg.Channels,
		Now: time.Now, Metrics: rec, Logger: log,
	}
}

// slackRecorder is factory_slack_requests_total{route, outcome}.
type slackRecorder struct{ requests metric.Int64Counter }

func newSlackRecorder(exp *metrics.Exporter) (*slackRecorder, error) {
	c, err := exp.Meter().Int64Counter("factory_slack_requests_total",
		metric.WithDescription("Requests the Slack App handled, by route (events, webhooks) and outcome."))
	if err != nil {
		return nil, fmt.Errorf("metrics: factory_slack_requests_total: %w", err)
	}
	return &slackRecorder{requests: c}, nil
}

func (r *slackRecorder) Request(ctx context.Context, route, outcome string) {
	r.requests.Add(ctx, 1, metric.WithAttributes(attribute.String("route", route), attribute.String("outcome", outcome)))
}
