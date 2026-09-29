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
// (PostgreSQL answers and the watch has synced) and /startupz (the schema is
// migrated, so a pod waits for Atlas instead of crash-looping).
func opsHandler(ping func(context.Context) error, schemaReady func(context.Context) (bool, error),
	synced func(context.Context) bool, metrics http.Handler,
) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", metrics)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok " + version.Version + "\n"))
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), probeTimeout)
		defer cancel()
		if err := ping(ctx); err != nil {
			http.Error(w, "database unreachable", http.StatusServiceUnavailable)
			return
		}
		if !synced(ctx) {
			http.Error(w, "watch not synced", http.StatusServiceUnavailable)
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

// syncWaiter is the one cache method readiness needs; a manager's cache has it.
type syncWaiter interface {
	WaitForCacheSync(ctx context.Context) bool
}

// syncCheck bounds readiness's wait on the watch. A synced cache answers at once;
// an already-ended context would not do: the cache then picks at random between
// "started" and "ended".
const syncCheck = 250 * time.Millisecond

// cacheSynced reports whether the watch has synced, waiting at most syncCheck.
func cacheSynced(c syncWaiter) func(context.Context) bool {
	return func(ctx context.Context) bool {
		ctx, cancel := context.WithTimeout(ctx, syncCheck)
		defer cancel()
		return c.WaitForCacheSync(ctx)
	}
}
