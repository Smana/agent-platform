// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Smana/maku/internal/factory/config"
	"github.com/Smana/maku/internal/factory/forge"
)

// /readyz on every replica, leader or not: the caches synced. GitHub is not a readiness input
// (#58): an outage must degrade the factory, not unready every replica.
func TestFactoryReadiness(t *testing.T) {
	for _, c := range []struct {
		name   string
		synced bool
		path   string
		want   int
	}{
		{"ready", true, "/readyz", http.StatusOK},
		{"not ready before the caches synced", false, "/readyz", http.StatusServiceUnavailable},
		{"started once the config parsed: the server starts after it", false, "/startupz", http.StatusOK},
		{"alive whatever GitHub says", false, "/healthz", http.StatusOK},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := factoryOps(func(context.Context) bool { return c.synced }, func() bool { return false }, http.NotFoundHandler())
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, c.path, nil))
			if rec.Code != c.want {
				t.Fatalf("GET %s = %d, want %d", c.path, rec.Code, c.want)
			}
		})
	}
}

// #57: the merger key is optional while no class needs it, and then the merger fails closed.
func TestConnectMerger(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "app_id")
	for _, c := range []struct {
		name    string
		idFile  string
		needs   bool
		wantErr bool
	}{
		{"no pair, nothing needs it", "", false, false},
		{"pair named, Secret absent, nothing needs it", missing, false, false},
		{"pair named, Secret absent, a shadow class needs it", missing, true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			cfg := &config.Config{Repository: "Smana/cloud-native-ref",
				GitHub: config.GitHub{MergerAppIDFile: c.idFile, MergerKeyFile: c.idFile}}
			if c.needs {
				cfg.Classes = map[string]config.Class{"docs-links": {Shadow: true}}
			}
			m, err := connectMerger(t.Context(), slog.New(slog.DiscardHandler), cfg, "Smana/cloud-native-ref")
			if c.wantErr {
				if err == nil {
					t.Fatal("a needed merger key that is missing must stop the start")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := m.PullRequestChecks(t.Context(), 1); !errors.Is(err, forge.ErrNoMerger) {
				t.Fatalf("an absent merger fails closed: %v", err)
			}
			if err := m.Merge(t.Context(), "n", "sha"); !errors.Is(err, forge.ErrNoMerger) {
				t.Fatalf("an absent merger never merges: %v", err)
			}
		})
	}
}
