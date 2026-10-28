// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/coder/websocket"

	"github.com/Smana/maku/internal/authn"
	"github.com/Smana/maku/internal/config"
	"github.com/Smana/maku/internal/fanout"
	"github.com/Smana/maku/internal/policy"
	"github.com/Smana/maku/internal/wire"
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

// A local install has no token, so the Origin check is the only CSRF gate, and it covers every
// route: the authenticated API, the public discovery and the static page alike. OIDC installs no
// such gate and keeps its discovery answer, with its own authenticator's check on the API.
func TestLocalHumanOriginPolicy(t *testing.T) {
	index := fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<!doctype html><html>rooms</html>")}}
	local := "http://localhost:8080"
	e := setup(t, func(s *Server, _ *fanout.Hub, _ *hubView) {
		s.Humans = authn.NewLocal(local, []string{"admin", "member"})
		s.Groups = policy.Groups{Admin: "admin", Member: "member"}
		s.WebClient = func() string { return authn.LocalClientID }
		s.PublicURL, s.HumanMode, s.Origins, s.UI = local, config.HumanModeLocal, authn.LoopbackOrigins(local), index
	})
	get := func(e env, path, origin string) (int, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, e.ts.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "localhost:8080" // the browser's, for the port-forward
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
	for _, path := range []string{"/api/rooms", "/.well-known/roomctl", "/"} {
		if code, body := get(e, path, ""); code != http.StatusOK {
			t.Fatalf("%s with no token and no Origin: %d %s", path, code, body)
		}
		if code, _ := get(e, path, local); code != http.StatusOK {
			t.Errorf("%s from the port-forward's own origin: %d", path, code)
		}
		for _, origin := range []string{"https://evil.example", "null", "http://localhost:8081"} {
			if code, _ := get(e, path, origin); code != http.StatusForbidden {
				t.Errorf("%s from origin %q: %d, want 403 before the route", path, origin, code)
			}
		}
	}
	if _, body := get(e, "/.well-known/roomctl", ""); !strings.Contains(body, `"humanMode":"local"`) {
		t.Fatalf("local discovery: %s", body)
	}

	// OIDC: no every-route gate, and discovery says which mode roomctl is talking to.
	o := setup(t, func(s *Server, _ *fanout.Hub, _ *hubView) {
		s.PublicURL, s.Issuer, s.HumanMode = "https://rooms.example", "https://auth.example", config.HumanModeOIDC
		s.RoomctlClient, s.ProjectID, s.UI = func() string { return "cli" }, func() string { return "proj" }, index
	})
	for _, origin := range []string{"", "https://evil.example"} {
		code, body := get(o, "/.well-known/roomctl", origin)
		if code != http.StatusOK || !strings.Contains(body, `"humanMode":"oidc"`) || !strings.Contains(body, `"clientID":"cli"`) ||
			!strings.Contains(body, `"projectID":"proj"`) {
			t.Fatalf("oidc discovery from %q: %d %s", origin, code, body)
		}
	}
	// No route-level gate in OIDC mode; the Origin check an OIDC install relies on is the
	// authenticator's own (internal/authn's Humans), which every authenticated route calls.
	if code, _ := get(o, "/api/rooms", "https://evil.example"); code == http.StatusForbidden {
		t.Fatal("oidc mode installed the local every-route Origin gate")
	}
}

// A non-browser client sends no Origin: with the local gate installed, the upgrade must still reach
// the local principal, and only a foreign Origin is refused, before the socket is opened.
func TestLocalHumanWebSocketOrigin(t *testing.T) {
	const local = "http://localhost:8080"
	e := setup(t, func(s *Server, _ *fanout.Hub, _ *hubView) {
		s.Humans = authn.NewLocal(local, []string{"admin", "member"})
		s.Groups = policy.Groups{Admin: "admin", Member: "member"}
		s.WebClient = func() string { return authn.LocalClientID }
		s.HumanMode, s.Origins = config.HumanModeLocal, authn.LoopbackOrigins(local)
	})
	addr := e.ts.Listener.Addr().String()
	dialer := &net.Dialer{}
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, network, addr)
	}}}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, "ws://localhost:8080/v1/ws?room="+roomID, &websocket.DialOptions{HTTPClient: client})
	if err != nil {
		if resp != nil {
			defer func() { _ = resp.Body.Close() }()
		}
		t.Fatalf("no Origin must upgrade: %v", err)
	}
	defer func() { _ = c.CloseNow() }()
	send(t, c, hello(nil, 0))
	if f := read(t, c); f.Type != wire.FrameState || f.Snapshot == nil || f.Snapshot.You.Role == "" {
		t.Fatalf("the local principal's state frame: %+v", f)
	}
	_, resp, err = websocket.Dial(ctx, "ws://localhost:8080/v1/ws?room="+roomID,
		&websocket.DialOptions{HTTPClient: client, HTTPHeader: http.Header{"Origin": {"https://evil.example"}}})
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("a foreign Origin must be refused before the upgrade: resp %v err %v", resp, err)
	}
	_ = resp.Body.Close()
}

// The page reached as http://127.0.0.1:8080 or http://[::1]:8080 is the same listener: it loads,
// lists and upgrades; a rebound Host still gets 403 on both.
func TestLocalLoopbackAliasesThroughServer(t *testing.T) {
	const local = "http://localhost:8080"
	e := setup(t, func(s *Server, _ *fanout.Hub, _ *hubView) {
		s.Humans = authn.NewLocal(local, []string{"admin", "member"})
		s.Groups = policy.Groups{Admin: "admin", Member: "member"}
		s.WebClient = func() string { return authn.LocalClientID }
		s.HumanMode, s.Origins = config.HumanModeLocal, authn.LoopbackOrigins(local)
	})
	for _, tc := range []struct {
		host, origin string
		want         int
	}{
		{"127.0.0.1:8080", "http://127.0.0.1:8080", http.StatusOK},
		{"[::1]:8080", "http://[::1]:8080", http.StatusOK},
		{"127.0.0.1:8080", "", http.StatusOK},
		{"evil.example:8080", "", http.StatusForbidden},
		{"127.0.0.1:8080", "http://127.0.0.1:9999", http.StatusForbidden},
	} {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, e.ts.URL+"/api/rooms", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = tc.host
		if tc.origin != "" {
			req.Header.Set("Origin", tc.origin)
		}
		resp, err := e.ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("host %q origin %q: %d, want %d", tc.host, tc.origin, resp.StatusCode, tc.want)
		}
	}
	addr := e.ts.Listener.Addr().String()
	dialer := &net.Dialer{}
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, network, addr)
	}}}
	for _, tc := range []struct {
		url, origin string
		ok          bool
	}{
		{"ws://127.0.0.1:8080/v1/ws?room=" + roomID, "http://127.0.0.1:8080", true},
		{"ws://[::1]:8080/v1/ws?room=" + roomID, "http://[::1]:8080", true},
		{"ws://127.0.0.1:8080/v1/ws?room=" + roomID, "https://127.0.0.1:8080", false},
		{"ws://evil.example:8080/v1/ws?room=" + roomID, "", false},
		{"ws://localhost.evil.example:8080/v1/ws?room=" + roomID, "", false},
	} {
		hdr := http.Header{}
		if tc.origin != "" {
			hdr.Set("Origin", tc.origin)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		c, resp, err := websocket.Dial(ctx, tc.url, &websocket.DialOptions{HTTPClient: client, HTTPHeader: hdr})
		cancel()
		if c != nil {
			_ = c.CloseNow()
		}
		refused := resp != nil && resp.StatusCode == http.StatusForbidden
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if tc.ok != (err == nil) || (!tc.ok && !refused) {
			t.Errorf("%s origin %s: err %v refused %v", tc.url, tc.origin, err, refused)
		}
	}
}
