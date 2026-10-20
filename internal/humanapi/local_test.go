// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/fanout"
	"github.com/Smana/agent-platform/internal/policy"
)

// A local-mode server end to end: the principal must pass Server.principal (a zero Expiry is
// refused), be the web UI, and be refused on a foreign Host (DNS rebinding) or Origin.
func TestLocalModeThroughServer(t *testing.T) {
	e := setup(t, func(s *Server, _ *fanout.Hub, _ *hubView) {
		s.Humans = authn.NewLocal("http://localhost:8080", []string{"admin", "member"})
		s.Groups = policy.Groups{Admin: "admin", Member: "member"}
		s.WebClient = func() string { return authn.LocalClientID }
	})
	get := func(host, origin string) (int, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, e.ts.URL+"/api/rooms", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = host
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := e.ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	code, body := get("localhost:8080", "")
	if code != http.StatusOK || !strings.Contains(body, `"webUI":true`) {
		t.Fatalf("local user, no Origin: %d %s", code, body)
	}
	if code, _ := get("evil.example:8080", ""); code != http.StatusForbidden {
		t.Fatalf("foreign Host (DNS rebinding): %d", code)
	}
	if code, _ := get("localhost:8080", "https://evil.example"); code != http.StatusForbidden {
		t.Fatalf("foreign Origin: %d", code)
	}
	// Dial as the allowed Host, with an Origin the websocket library's own same-host check accepts
	// (same host, other scheme): only the broker's exact-Origin check can refuse it.
	addr := e.ts.Listener.Addr().String()
	dialer := &net.Dialer{}
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, network, addr)
	}}}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, "ws://localhost:8080/v1/ws?room="+roomID,
		&websocket.DialOptions{HTTPClient: client, HTTPHeader: http.Header{"Origin": {"https://localhost:8080"}}})
	if c != nil {
		_ = c.CloseNow()
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("WebSocket from a foreign Origin: resp %v err %v", resp, err)
	}
	_ = resp.Body.Close()
}
