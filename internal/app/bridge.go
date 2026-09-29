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
	"sync"
	"time"

	"github.com/Smana/agent-platform/internal/bridge"
	"github.com/Smana/agent-platform/internal/version"
)

// The bridge's defaults (docs/integration.md, "The bridge's environment").
const (
	defaultHarnessURL = "http://127.0.0.1:8000"
	defaultHealthAddr = ":8085"
	defaultBrokerCA   = "/etc/room-broker-ca/ca.crt" // GP-18: the room-broker-ca mount
	// healthDrain bounds the health server's shutdown, after the bridge's own
	// flush and inside the pod's 30 s grace.
	healthDrain = 2 * time.Second
)

type bridgeConfig struct {
	roomID, runID, conversationID  string
	brokerURL, brokerCA, tokenFile string
	harnessURL, healthAddr         string
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
	var missing []error
	for _, kv := range [][2]string{{"ROOM_ID", c.roomID}, {"RUN_ID", c.runID}, {"CONVERSATION_ID", c.conversationID},
		{"BROKER_URL", c.brokerURL}, {"ROOM_TOKEN_FILE", c.tokenFile}} {
		if kv[1] == "" {
			missing = append(missing, fmt.Errorf("%s is not set", kv[0]))
		}
	}
	return c, errors.Join(missing...)
}

// healthHandler serves /healthz for the kubelet only (ruling P6).
func healthHandler(healthy func(time.Time) bool, now func() time.Time) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		if !healthy(now()) {
			http.Error(w, "harness unreachable", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ok " + version.Version + "\n"))
	})
	return mux
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
	b := &bridge.Bridge{Harness: bridge.NewHarness(cfg.harnessURL, cfg.conversationID), Broker: broker,
		RunID: cfg.runID, Logger: log}

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.healthAddr)
	if err != nil {
		return fmt.Errorf("room-bridge: health listener: %w", err)
	}
	srv := &http.Server{Handler: healthHandler(b.Healthy, time.Now), ReadHeaderTimeout: 5 * time.Second,
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
