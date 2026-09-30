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

// roomMCP is the :8090 room tools server over the broker's parts (§3): the run
// comes from the router's verified subject, matched by the run issuer's pattern.
func roomMCP(key string, subPattern *regexp.Regexp, roomLog mcp.Log, red mcp.Redactor, runs mcp.Runs,
	m *metrics.Set, log *slog.Logger,
) *mcp.Server {
	return &mcp.Server{Key: func() string { return key }, Runs: runs, SubPattern: subPattern,
		Tools: mcp.RoomTools(roomLog, red, time.Now),
		OnReject: func(ctx context.Context, reason string) {
			m.Rejected.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", reason)))
		},
		Logger: log, Now: time.Now}
}

// mcpServer bounds :8090. Nothing on it streams, so WriteTimeout is set, and
// each call's context ends at mcpCallTimeout.
func mcpServer(h http.Handler, log *slog.Logger) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/mcp", http.TimeoutHandler(h, mcpCallTimeout, `{"jsonrpc":"2.0","id":null,"error":{"code":-32603,"message":"timed out"}}`))
	return &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: mcpCallTimeout + 5*time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10,
		ErrorLog: slog.NewLogLogger(log.Handler(), slog.LevelWarn)}
}
