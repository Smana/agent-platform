// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Smana/maku/internal/fanout"
)

// mcpOn serves /mcp, maku's client id being "maku".
func mcpOn(s *Server, _ *fanout.Hub, _ *hubView) {
	s.MCP, s.RoomctlClient = true, func() string { return "maku" }
}

// viaRoomctl is user's request with maku's token, with extra header pairs.
func viaRoomctl(user string, kv ...string) http.Header {
	return header(user, append([]string{"X-Test-Client", "maku"}, kv...)...)
}

// post sends body to /mcp with h; it returns the status, the Content-Type and the body.
func post(t *testing.T, e env, h http.Header, body string) (int, string, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, e.ts.URL+"/mcp", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header = h.Clone()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	r, err := e.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Body.Close() }()
	b, _ := io.ReadAll(r.Body)
	return r.StatusCode, r.Header.Get("Content-Type"), string(b)
}

// rpc sends one JSON-RPC request; it returns the status and the response object, read from the
// JSON body, or from the last data line of a stream.
func rpc(t *testing.T, e env, h http.Header, method string, params any) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	code, ct, raw := post(t, e, h, string(body))
	if strings.HasPrefix(ct, "text/event-stream") {
		sc := bufio.NewScanner(strings.NewReader(raw))
		for sc.Scan() {
			if d, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
				raw = d
			}
		}
	}
	var out map[string]any
	_ = json.NewDecoder(bytes.NewReader([]byte(raw))).Decode(&out)
	return code, out
}

// rpcCode is a JSON-RPC error's code, or 0.
func rpcCode(out map[string]any) int {
	if e, ok := out["error"].(map[string]any); ok {
		return int(e["code"].(float64))
	}
	return 0
}

// D4: /mcp is off unless the broker's config turns it on.
func TestMCPIsOffUnlessConfigured(t *testing.T) {
	e := setup(t)
	if code, _ := rpc(t, e, viaRoomctl("dev"), "ping", nil); code != http.StatusNotFound {
		t.Fatalf("off: %d", code)
	}
}

// Only maku's token drives /mcp: a web session never carries its standing there.
func TestMCPTakesRoomctlTokensOnly(t *testing.T) {
	e := setup(t, mcpOn)
	for name, c := range map[string]struct {
		h    http.Header
		want int
	}{
		"anonymous":              {header(""), http.StatusUnauthorized},
		"a web session":          {header("dev"), http.StatusForbidden},
		"another client":         {header("dev", "X-Test-Client", "other"), http.StatusForbidden},
		"not in an agents group": {viaRoomctl("stranger"), http.StatusForbidden},
		"maku":                   {viaRoomctl("dev"), http.StatusOK},
	} {
		t.Run(name, func(t *testing.T) {
			if code, _ := rpc(t, e, c.h, "ping", nil); code != c.want {
				t.Fatalf("%d, want %d", code, c.want)
			}
		})
	}
	if !strings.Contains(scrape(t, e), `rooms_rejected_actions_total{reason="mcp_needs_roomctl"} 2`) {
		t.Fatal("refusals uncounted")
	}
	noClient := setup(t, func(s *Server, _ *fanout.Hub, _ *hubView) { s.MCP = true })
	if code, _ := rpc(t, noClient, viaRoomctl("dev"), "ping", nil); code != http.StatusForbidden {
		t.Fatalf("a broker without a maku client admits nobody: %d", code)
	}
	same := setup(t, mcpOn, func(s *Server, _ *fanout.Hub, _ *hubView) { s.WebClient = func() string { return "maku" } })
	if code, _ := rpc(t, same, viaRoomctl("dev"), "ping", nil); code != http.StatusForbidden {
		t.Fatalf("a maku client that is the web client admits nobody: %d", code)
	}
}

func TestMCPInitialize(t *testing.T) {
	e := setup(t, mcpOn)
	for asked, want := range map[string]string{"2025-06-18": "2025-06-18", "2025-11-25": "2025-11-25", "2026-07-28": "2025-11-25", "": "2025-11-25"} {
		_, out := rpc(t, e, viaRoomctl("dev"), "initialize", map[string]any{"protocolVersion": asked, "capabilities": map[string]any{}})
		res, _ := out["result"].(map[string]any)
		if res["protocolVersion"] != want || res["serverInfo"].(map[string]any)["name"] != "maku-broker" ||
			!strings.Contains(fmt.Sprint(res["instructions"]), "untrusted: true") {
			t.Errorf("asked %q: %v", asked, out)
		}
	}
}

// A modern client's per-request version gets a 400 with no modern error code: a dual-era client
// falls back to initialize.
func TestMCPProtocolVersionHeader(t *testing.T) {
	e := setup(t, mcpOn)
	code, out := rpc(t, e, viaRoomctl("dev", "MCP-Protocol-Version", "2026-07-28"), "ping", nil)
	if code != http.StatusBadRequest || rpcCode(out) != -32600 || fmt.Sprint(out["error"].(map[string]any)["data"]) != "map[supported:[2025-06-18 2025-11-25]]" {
		t.Fatalf("%d %v", code, out)
	}
	if code, _ := rpc(t, e, viaRoomctl("dev", "MCP-Protocol-Version", "2025-06-18"), "ping", nil); code != http.StatusOK {
		t.Fatalf("a served revision: %d", code)
	}
}

func TestMCPMethods(t *testing.T) {
	e := setup(t, mcpOn)
	h := viaRoomctl("dev")
	if _, out := rpc(t, e, h, "ping", nil); fmt.Sprint(out["result"]) != "map[]" {
		t.Fatalf("ping: %v", out)
	}
	if _, out := rpc(t, e, h, "tools/list", nil); out["result"].(map[string]any)["tools"] == nil {
		t.Fatalf("tools/list: %v", out)
	}
	for method, want := range map[string]int{"resources/list": -32601, "server/discover": -32601, "tasks/list": -32601} {
		if _, out := rpc(t, e, h, method, map[string]any{}); rpcCode(out) != want {
			t.Errorf("%s: %v", method, out)
		}
	}
	if _, out := rpc(t, e, h, "tools/call", map[string]any{"name": "nope"}); rpcCode(out) != -32602 {
		t.Fatalf("an unknown tool: %v", out)
	}
	if code, _, body := post(t, e, h, `{"jsonrpc":"2.0","method":"notifications/initialized"}`); code != http.StatusAccepted || body != "" {
		t.Fatalf("a notification: %d %q", code, body)
	}
	if _, _, body := post(t, e, h, `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`); !strings.Contains(body, `"code":-32700`) {
		t.Fatalf("a batch: %s", body)
	}
	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, e.ts.URL+"/mcp", nil)
	req.Header = h
	r, err := e.ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = r.Body.Close()
	if r.StatusCode != http.StatusMethodNotAllowed || r.Header.Get("Allow") != http.MethodPost {
		t.Fatalf("GET: %d %q", r.StatusCode, r.Header.Get("Allow"))
	}
}

// Per replica, as acts: 10 requests a second, burst 20.
func TestMCPIsRateLimited(t *testing.T) {
	e := setup(t, mcpOn)
	limited := false
	for range 40 {
		if _, out := rpc(t, e, viaRoomctl("dev"), "ping", nil); strings.HasPrefix(fmt.Sprint(out["error"]), "map[code:-32603 message:rate_limited") {
			limited = true
			break
		}
	}
	if !limited {
		t.Fatal("40 requests at once were all served")
	}
}
