// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/Smana/agent-platform/internal/version"
)

// probeTimeout bounds each probe's check, under the kubelet's default 1 s timeout
// plus a margin the kubelet can be told to wait.
const probeTimeout = 2 * time.Second

// opsHandler serves :9090: /metrics, /healthz (the process answers), /readyz
// (not draining, PostgreSQL answers and the run watch holds every run) and
// /startupz (the schema is migrated, so a pod waits for Atlas instead of
// crash-looping).
func opsHandler(ping func(context.Context) error, schemaReady func(context.Context) (bool, error),
	synced func(context.Context) bool, draining func() bool, metrics http.Handler,
) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", metrics)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok " + version.Version + "\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if draining() {
			http.Error(w, "shutting down", http.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
		defer cancel()
		if err := ping(ctx); err != nil {
			http.Error(w, "database unreachable", http.StatusServiceUnavailable)
			return
		}
		if !synced(ctx) {
			http.Error(w, "run watch not synced", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ready\n"))
	})
	mux.HandleFunc("GET /startupz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
		defer cancel()
		if ok, err := schemaReady(ctx); err != nil || !ok {
			http.Error(w, "schema not migrated", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("started\n"))
	})
	return mux
}

// opsServer bounds :9090. Nothing on it streams, so WriteTimeout is set.
func opsServer(h http.Handler, log *slog.Logger) *http.Server {
	return &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second,
		WriteTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 8 << 10,
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn)}
}

// syncCheck bounds readiness's wait on the run watch: a synced one answers at once.
const syncCheck = 250 * time.Millisecond

// boundedSync waits on synced for at most syncCheck.
func boundedSync(synced func(context.Context) bool) func(context.Context) bool {
	return func(ctx context.Context) bool {
		ctx, cancel := context.WithTimeout(ctx, syncCheck)
		defer cancel()
		return synced(ctx)
	}
}
