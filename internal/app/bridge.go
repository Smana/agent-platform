// SPDX-License-Identifier: Apache-2.0

// Package app wires each binary: it reads the environment, builds the adapters
// and servers, and runs them until the root context ends. It is the only
// importer of every adapter; cmd/<bin>/main.go only builds the root context and
// logger and calls app.Run<Bin>.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/Smana/agent-platform/internal/bridge"
	"github.com/Smana/agent-platform/internal/httpx"
	"github.com/Smana/agent-platform/internal/version"
	"github.com/Smana/agent-platform/internal/wire"
)

// The bridge's defaults (docs/integration.md, "The bridge's environment").
const (
	defaultHarnessURL = "http://127.0.0.1:8000"
	defaultHealthAddr = ":8085"
	defaultBrokerCA   = "/etc/room-broker-ca/ca.crt" // GP-18: the room-broker-ca mount
	// healthDrain bounds the health server's shutdown, after the bridge's own
	// flush and inside the pod's 30 s grace.
	healthDrain = 2 * time.Second
	// maxFlushGrace keeps the drain inside the pod's 30 s grace, with room for
	// the "unmirrored" Warn line before the kubelet's SIGKILL.
	maxFlushGrace = 28 * time.Second
)

type bridgeConfig struct {
	roomID, runID, conversationID  string
	brokerURL, brokerCA, tokenFile string
	harnessURL, healthAddr         string
	flushGrace                     time.Duration // 0: the bridge's default
	memLimitSet                    bool          // GOMEMLIMIT is in the environment
	// branch is the run's branch (CC-S5). Unset, every push is forge.other.
	branch string
	egress map[string]bool // the run's egress profiles
}

func loadBridgeConfig(getenv func(string) string) (bridgeConfig, error) {
	or := func(k, def string) string {
		if v := getenv(k); v != "" {
			return v
		}
		return def
	}
	c := bridgeConfig{roomID: getenv("ROOM_ID"), runID: getenv("RUN_ID"), conversationID: getenv("CONVERSATION_ID"),
		brokerURL: getenv("BROKER_URL"), brokerCA: or("BROKER_CA_FILE", defaultBrokerCA),
		tokenFile: getenv("ROOM_TOKEN_FILE"), harnessURL: or("HARNESS_URL", defaultHarnessURL),
		healthAddr: or("HEALTH_ADDR", defaultHealthAddr)}
	c.memLimitSet = getenv("GOMEMLIMIT") != ""
	c.branch, c.egress = getenv("BRANCH"), map[string]bool{}
	for p := range strings.SplitSeq(getenv("EGRESS_PROFILES"), ",") {
		if p = strings.TrimSpace(p); p != "" {
			c.egress[p] = true
		}
	}
	var missing []error
	if v := getenv("FLUSH_GRACE"); v != "" {
		d, err := time.ParseDuration(v)
		switch {
		case err != nil || d <= 0:
			missing = append(missing, fmt.Errorf("FLUSH_GRACE %q is not a positive duration", v))
		case d > maxFlushGrace:
			missing = append(missing, fmt.Errorf("FLUSH_GRACE %s must be at most %s, inside the pod's 30s grace", d, maxFlushGrace))
		}
		c.flushGrace = d
	}
	for _, kv := range [][2]string{{"ROOM_ID", c.roomID}, {"RUN_ID", c.runID}, {"CONVERSATION_ID", c.conversationID},
		{"BROKER_URL", c.brokerURL}, {"ROOM_TOKEN_FILE", c.tokenFile}} {
		if kv[1] == "" {
			missing = append(missing, fmt.Errorf("%s is not set", kv[0]))
		}
	}
	return c, errors.Join(missing...)
}

// healthHandler serves /healthz for the kubelet only (ruling P6), and
// /admission for room-bridge gate on loopback (F15): 503 while the first hellos
// are undecided, 200 once the run holds the room, 409 and the reason once it
// never will.
func healthHandler(healthy func(time.Time) bool, admission func() bridge.Admission, now func() time.Time) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		if !healthy(now()) {
			http.Error(w, "harness unreachable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok " + version.Version + "\n"))
	})
	mux.HandleFunc("GET /admission", func(w http.ResponseWriter, _ *http.Request) {
		switch a := admission(); {
		case a.Admitted:
			_, _ = w.Write([]byte("admitted\n"))
		case a.Refused != "":
			http.Error(w, a.Refused, http.StatusConflict)
		default:
			http.Error(w, "pending", http.StatusServiceUnavailable)
		}
	})
	return mux
}

// wireBridge sets the bridge's hooks: steering (phase 4) and the confirmation
// loop (phase 5). A hook left unset fails open or wedges every run, so the
// wiring is tested on its own (review M7).
func wireBridge(b *bridge.Bridge, approvals bridge.ApprovalRequester, cfg bridgeConfig,
	log *slog.Logger,
) (*bridge.Confirmer, *bridge.Steering) {
	confirm := &bridge.Confirmer{Harness: b.Harness, Broker: approvals, RunID: b.RunID, Push: b.Push, Logger: log,
		Classifier: bridge.Classifier{Branch: cfg.branch, Egress: cfg.egress}}
	steer := &bridge.Steering{Harness: b.Harness, RunID: b.RunID, Push: b.Push, Gate: confirm.Gate}
	b.OnDeliver, b.OnInterrupt = steer.Deliver, steer.Interrupt
	b.OnResume = func(_ context.Context, r wire.Resume) { confirm.SetPolicy(r.Approvals) }
	b.OnReady, b.OnRaw, b.OnStatus, b.OnDecision = confirm.Ready, confirm.Observe, confirm.OnStatus, confirm.Decision
	b.Classify = confirm.ClassOf
	return confirm, steer
}

// gatePoll is how often room-bridge gate asks the bridge, and gateTimeout
// bounds one question.
const (
	gatePoll    = time.Second
	gateTimeout = 2 * time.Second
)

// ErrNotAdmitted is RunGate's failure when the bridge will never hold the room.
var ErrNotAdmitted = errors.New("the room refused this run")

// RunGate runs room-bridge gate, the init container between the bridge and the
// harness (F15). It returns nil once the bridge holds the room's lease, so the
// harness may start, and an error once the room refuses the run or ctx ends:
// the pod's restartPolicy Never then fails the run before the harness runs. It
// reads the bridge's /admission on loopback at HEALTH_ADDR's port.
func RunGate(ctx context.Context, log *slog.Logger, getenv func(string) string) error {
	addr := getenv("HEALTH_ADDR")
	if addr == "" {
		addr = defaultHealthAddr
	}
	return gate(ctx, log, "http://"+loopback(addr)+"/admission", gatePoll)
}

func gate(ctx context.Context, log *slog.Logger, url string, poll time.Duration) error {
	hc := httpx.New(gateTimeout, nil)
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		code, body, err := getAdmission(ctx, hc, url)
		switch {
		case err == nil && code == http.StatusOK:
			log.Info("the bridge holds the room; the harness may start")
			return nil
		case err == nil && code == http.StatusConflict:
			return fmt.Errorf("room-bridge gate: %w: %s", ErrNotAdmitted, body)
		case err != nil:
			log.Debug("the bridge is not answering yet", "err", err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("room-bridge gate: the bridge never decided: %w", ctx.Err())
		case <-t.C:
		}
	}
}

func getAdmission(ctx context.Context, hc *http.Client, url string) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, "", err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := httpx.ReadBody(resp.Body, 256)
	return resp.StatusCode, strings.TrimSpace(string(b)), err
}

// loopback is addr's port on 127.0.0.1 when addr listens on every interface.
func loopback(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if ip := net.ParseIP(host); host == "" || (ip != nil && ip.IsUnspecified()) {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}

// RunBridge runs room-bridge, the native sidecar of an AgentRun sandbox with a
// roomRef, until ctx ends, then flushes what it holds. getenv reads its
// environment (docs/integration.md). It fails before starting on a missing
// variable, a BROKER_URL that is not https:// or a BROKER_CA_FILE with no CA
// (GP-18), or a HEALTH_ADDR it cannot listen on.
func RunBridge(ctx context.Context, log *slog.Logger, getenv func(string) string) error {
	cfg, err := loadBridgeConfig(getenv)
	if err != nil {
		return fmt.Errorf("room-bridge: %w", err)
	}
	broker, err := bridge.NewBroker(cfg.brokerURL, cfg.tokenFile, cfg.brokerCA)
	if err != nil {
		return fmt.Errorf("room-bridge: %w", err)
	}
	log = log.With("run", cfg.runID, "room", cfg.roomID)
	if !cfg.memLimitSet {
		// The 64 Mi sidecar's soft limit (review I2), unless the pod spec sets
		// GOMEMLIMIT itself; the runtime reads that one at start.
		defer debug.SetMemoryLimit(debug.SetMemoryLimit(bridge.MemoryLimit))
	}
	b := &bridge.Bridge{Harness: bridge.NewHarness(cfg.harnessURL, cfg.conversationID), Broker: broker,
		RunID: cfg.runID, Logger: log, FlushGrace: cfg.flushGrace}
	_, _ = wireBridge(b, broker, cfg, log)

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.healthAddr)
	if err != nil {
		return fmt.Errorf("room-bridge: health listener: %w", err)
	}
	srv := &http.Server{Handler: healthHandler(b.Healthy, b.Admission, time.Now), ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8 << 10}
	var wg sync.WaitGroup
	wg.Go(func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("health server", "err", err)
		}
	})
	log.Info("room-bridge starting", "version", version.Version)
	runErr := b.Run(ctx)
	drain, cancel := context.WithTimeout(context.WithoutCancel(ctx), healthDrain)
	defer cancel()
	if err := srv.Shutdown(drain); err != nil {
		log.Warn("health server shutdown", "err", err)
	}
	wg.Wait()
	return runErr
}
