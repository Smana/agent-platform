// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/Smana/agent-platform/internal/config"
	"github.com/Smana/agent-platform/internal/mcp"
	"github.com/Smana/agent-platform/internal/metrics"
)

const (
	mcpAddr = ":8090"
	// mcpCallTimeout bounds one tool call, the store read or append and the
	// redaction included; its context ends with it.
	mcpCallTimeout = 15 * time.Second
	mcpDrain       = 10 * time.Second
)

// mcpKey is the key the MCPRoute injects (ruling P13). Unset, :8090 refuses
// every call rather than admit one that sends no key.
func mcpKey(getenv func(string) string) string {
	return strings.TrimSpace(getenv("ROOMS_MCP_KEY"))
}

// subPatterns are every run issuer's subject pattern: the router verified the
// run's token, and any issuer's run may call its tools (review M2).
func subPatterns(issuers []config.IssuerConfig) []*regexp.Regexp {
	out := make([]*regexp.Regexp, 0, len(issuers))
	for _, is := range issuers {
		out = append(out, regexp.MustCompile(is.SubPattern)) // config.Validate compiled it
	}
	return out
}

// roomMCP is the :8090 room tools server over the broker's parts (§3): the run
// comes from the router's verified subject, matched by a run issuer's pattern.
func roomMCP(key string, patterns []*regexp.Regexp, roomLog mcp.Log, red mcp.Redactor, runs mcp.Runs,
	m *metrics.Set, log *slog.Logger,
) *mcp.Server {
	return &mcp.Server{Key: func() string { return key }, Runs: runs, SubPatterns: patterns,
		Tools: mcp.RoomTools(roomLog, red, time.Now),
		OnReject: func(ctx context.Context, reason string) {
			m.Rejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", reason)))
		},
		Logger: log, Now: time.Now}
}

// mcpServer bounds :8090. Nothing on it streams, so WriteTimeout is set, and
// each call's context ends at callTimeout (mcpCallTimeout in the broker).
func mcpServer(h http.Handler, callTimeout time.Duration, log *slog.Logger) *http.Server {
	timed := http.TimeoutHandler(h, callTimeout, `{"jsonrpc":"2.0","id":null,"error":{"code":-32603,"message":"timed out"}}`)
	mux := http.NewServeMux()
	mux.Handle("/mcp", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// TimeoutHandler writes its 503 on w as is: this is its Content-Type (review M3).
		w.Header().Set("Content-Type", "application/json")
		timed.ServeHTTP(w, r)
	}))
	return &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: callTimeout + 5*time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10,
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn)}
}
