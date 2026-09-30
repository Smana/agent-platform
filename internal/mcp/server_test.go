// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/Smana/agent-platform/internal/policy"
	"github.com/Smana/agent-platform/internal/runwatch"
	"github.com/Smana/agent-platform/internal/store"
)

const sub = "system:serviceaccount:agents:xplane-run-7f3cq2xz"

func claim(id, role, room, phase string, annotations map[string]any) *unstructured.Unstructured {
	md := map[string]any{"name": "xplane-run-" + id, "namespace": "agents"}
	if annotations != nil {
		md["annotations"] = annotations
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"metadata": md,
		"spec":     map[string]any{"roomRef": room, "role": role},
		"status":   map[string]any{"phase": phase},
	}}
}

func watcher(t *testing.T, role string) *runwatch.Watcher {
	w := runwatch.New()
	w.Upsert(t.Context(), claim("7f3cq2xz", role, "3kq7x2ma", "Running", nil))
	return w
}

// clock is a settable Now.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newClock() *clock { return &clock{t: time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)} }

var (
	subPattern = regexp.MustCompile(`^system:serviceaccount:agents:xplane-run-([a-z2-7]{8})$`)
	objSchema  = json.RawMessage(`{"type":"object"}`)
)

func testServer(runs Runs, c *clock) (*Server, *[]string) {
	var mu sync.Mutex
	rejected := &[]string{}
	return &Server{Key: func() string { return "k" }, Runs: runs, SubPattern: subPattern, Now: c.now,
		OnReject: func(_ context.Context, reason string) { mu.Lock(); *rejected = append(*rejected, reason); mu.Unlock() },
		Tools: []Tool{
			{Name: "room_read", Action: policy.Read, Roles: []string{"implementer", "reviewer", "tester", "triager"}, InputSchema: objSchema,
				Call: func(context.Context, Caller, json.RawMessage) (any, error) { return map[string]int{"lastSeq": 3}, nil }},
			{Name: "room_verdict", Action: policy.Chat, Roles: []string{"reviewer", "tester"}, InputSchema: objSchema,
				Call: func(context.Context, Caller, json.RawMessage) (any, error) { return map[string]int{"seq": 4}, nil }},
			{Name: "room_sealed", Action: policy.Chat, Roles: []string{"reviewer"}, InputSchema: objSchema,
				Call: func(context.Context, Caller, json.RawMessage) (any, error) {
					return nil, fmt.Errorf("store: append to room 3kq7x2ma: %w", store.ErrSealed)
				}},
			{Name: "room_broken", Action: policy.Chat, Roles: []string{"reviewer"}, InputSchema: objSchema,
				Call: func(context.Context, Caller, json.RawMessage) (any, error) {
					return nil, errors.New("pq: invalid input syntax near 'ghp_secretvalue'")
				}},
			{Name: "room_badargs", Action: policy.Chat, Roles: []string{"reviewer"}, InputSchema: objSchema,
				Call: func(context.Context, Caller, json.RawMessage) (any, error) {
					return nil, argError("text: 1 to 16384 bytes")
				}},
			{Name: "room_noaction", Roles: []string{"reviewer"}, InputSchema: objSchema,
				Call: func(context.Context, Caller, json.RawMessage) (any, error) { return map[string]int{"seq": 5}, nil }},
		}}, rejected
}

func server(t *testing.T, role string) *Server {
	s, _ := testServer(watcher(t, role), newClock())
	return s
}

type response struct {
	status int
	body   []byte
	out    map[string]any
}

func post(t *testing.T, s *Server, key, identity string, body []byte) response {
	t.Helper()
	r := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/mcp", bytes.NewReader(body))
	r.Header.Set(KeyHeader, key)
	r.Header.Set(IdentityHeader, identity)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return response{status: rec.Code, body: rec.Body.Bytes(), out: out}
}

func rpc(t *testing.T, s *Server, key, identity, method string, params any) response {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	return post(t, s, key, identity, body)
}

func call(t *testing.T, s *Server, identity, name string) (map[string]any, string) {
	t.Helper()
	res, _ := rpc(t, s, "k", identity, "tools/call", map[string]any{"name": name, "arguments": map[string]any{}}).out["result"].(map[string]any)
	text := ""
	if c, ok := res["content"].([]any); ok && len(c) == 1 {
		text, _ = c[0].(map[string]any)["text"].(string)
	}
	return res, text
}

func TestOnlyTheRouterWithTheKeyAndAVerifiedRun(t *testing.T) {
	c := newClock()
	w := runwatch.New()
	w.Upsert(t.Context(), claim("7f3cq2xz", "reviewer", "3kq7x2ma", "Running", nil))
	w.Upsert(t.Context(), claim("done2345", "reviewer", "3kq7x2ma", "Succeeded", nil))
	w.Upsert(t.Context(), claim("revokedx", "reviewer", "3kq7x2ma", "Running", map[string]any{runwatch.RevokedAnnotation: "owner"}))
	w.Upsert(t.Context(), claim("noroom23", "reviewer", "", "Running", nil))
	s, rejected := testServer(w, c)
	unset, _ := testServer(w, c)
	unset.Key = func() string { return "" }
	loose, _ := testServer(w, c)
	loose.SubPattern = regexp.MustCompile(`xplane-run-([a-z2-7]{8})`) // unanchored: must still match the whole subject

	for _, tc := range []struct {
		name     string
		s        *Server
		key, id  string
		status   int
		rejectAs string
	}{
		{"the router's key and a live run", s, "k", sub, http.StatusOK, ""},
		{"a wrong key", s, "wrong", sub, http.StatusUnauthorized, "mcp_key"},
		{"no key", s, "", sub, http.StatusUnauthorized, "mcp_key"},
		{"an unset key refuses everyone, an empty header included", unset, "", sub, http.StatusUnauthorized, "mcp_key"},
		{"a subject that names no run", s, "k", "system:serviceaccount:agents:someone", http.StatusForbidden, "mcp_identity"},
		{"no subject", s, "k", "", http.StatusForbidden, "mcp_identity"},
		{"a subject that only contains a run's name", loose, "k", "evil:" + sub, http.StatusForbidden, "mcp_identity"},
		{"an unknown run", s, "k", "system:serviceaccount:agents:xplane-run-zzzzzzzz", http.StatusForbidden, "run_not_live"},
		{"a finished run", s, "k", "system:serviceaccount:agents:xplane-run-done2345", http.StatusForbidden, "run_not_live"},
		{"a revoked run", s, "k", "system:serviceaccount:agents:xplane-run-revokedx", http.StatusForbidden, "run_not_live"},
		{"a run in no room", s, "k", "system:serviceaccount:agents:xplane-run-noroom23", http.StatusForbidden, "run_not_live"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			*rejected = nil
			got := rpc(t, tc.s, tc.key, tc.id, "tools/list", nil)
			if got.status != tc.status {
				t.Fatalf("status = %d, want %d (%s)", got.status, tc.status, got.body)
			}
			if tc.s == s && tc.rejectAs != "" && (len(*rejected) != 1 || (*rejected)[0] != tc.rejectAs) {
				t.Fatalf("rejected = %v, want [%s]", *rejected, tc.rejectAs)
			}
		})
	}
}

// anyRun claims every id is a live run, so only the server's own check can
// refuse an id that is not a C2 id.
type anyRun struct{}

func (anyRun) Live(id string) (runwatch.Run, bool) {
	return runwatch.Run{ID: id, Room: "3kq7x2ma", Role: "reviewer", Phase: "Running"}, true
}

func TestTheSubjectMustNameAC2Id(t *testing.T) {
	s, _ := testServer(anyRun{}, newClock())
	s.SubPattern = regexp.MustCompile(`^system:serviceaccount:agents:xplane-run-(.+)$`)
	if got := rpc(t, s, "k", "system:serviceaccount:agents:xplane-run-NOT-AN-ID", "tools/list", nil); got.status != http.StatusForbidden {
		t.Fatalf("status = %d", got.status)
	}
}

func TestOnlyPost(t *testing.T) {
	s := server(t, "reviewer")
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/mcp", nil)
	r.Header.Set(KeyHeader, "k")
	r.Header.Set(IdentityHeader, sub)
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, r)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d: the server opens no stream of its own (agent-router#2715)", rec.Code)
	}
}

func TestToolsFollowTheRunsRoleNotAHeader(t *testing.T) {
	list := func(role string) string {
		b, _ := json.Marshal(rpc(t, server(t, role), "k", sub, "tools/list", nil).out["result"])
		return string(b)
	}
	if !strings.Contains(list("reviewer"), "room_verdict") {
		t.Fatal("a reviewer lists room_verdict")
	}
	if l := list("implementer"); strings.Contains(l, "room_verdict") || !strings.Contains(l, "room_read") {
		t.Fatalf("an implementer lists room_read and never room_verdict: %s", l)
	}
	if l := list("stranger"); l != `{"tools":[]}` {
		t.Fatalf("a role with no tools lists an empty array: %s", l)
	}
	s, rejected := testServer(watcher(t, "implementer"), newClock())
	res, text := call(t, s, sub, "room_verdict")
	if res["isError"] != true || !strings.HasPrefix(text, "not_permitted") {
		t.Fatalf("calling a tool the role lacks: %v", res)
	}
	if res, _ := call(t, s, sub, "room_nothing"); res["isError"] != true {
		t.Fatalf("calling an unknown tool: %v", res)
	}
	if strings.Join(*rejected, ",") != "tool_not_permitted,tool_not_permitted" {
		t.Fatalf("rejected = %v", *rejected)
	}
}

// A tool without a policy action is refused: the matrix is the one enforcement
// point (T7), so a tool that forgot to name its action fails closed.
func TestAToolWithoutAnActionIsRefused(t *testing.T) {
	res, _ := call(t, server(t, "reviewer"), sub, "room_noaction")
	if res["isError"] != true {
		t.Fatalf("%v", res)
	}
}

// Agent Router 1.1.0 authorizes tools/* only: anything else must not exist here (§3).
func TestNoResourcesNoPrompts(t *testing.T) {
	s := server(t, "reviewer")
	init := rpc(t, s, "k", sub, "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}})
	result, _ := init.out["result"].(map[string]any)
	caps, _ := result["capabilities"].(map[string]any)
	if _, ok := caps["tools"]; !ok {
		t.Fatalf("tools not advertised: %s", init.body)
	}
	if _, ok := caps["resources"]; ok {
		t.Fatal("resources advertised")
	}
	if _, ok := caps["prompts"]; ok {
		t.Fatal("prompts advertised")
	}
	for _, m := range []string{"resources/list", "resources/read", "prompts/list", "prompts/get", "sampling/createMessage"} {
		out := rpc(t, s, "k", sub, m, nil)
		if e, _ := out.out["error"].(map[string]any); e["code"] != float64(-32601) {
			t.Errorf("%s: %s", m, out.body)
		}
	}
}

func TestProtocolVersion(t *testing.T) {
	s := server(t, "reviewer")
	for asked, want := range map[string]string{"2025-06-18": "2025-06-18", "2025-11-25": "2025-11-25", "1999-01-01": "2026-07-28", "": "2026-07-28"} {
		out := rpc(t, s, "k", sub, "initialize", map[string]any{"protocolVersion": asked})
		if got := out.out["result"].(map[string]any)["protocolVersion"]; got != want {
			t.Errorf("asked %q, got %v, want %s", asked, got, want)
		}
	}
	if out := rpc(t, s, "k", sub, "ping", nil); out.out["result"] == nil {
		t.Fatalf("ping: %s", out.body)
	}
}

func TestJSONRPCFraming(t *testing.T) {
	s := server(t, "reviewer")
	for _, tc := range []struct {
		name   string
		body   string
		status int
		code   float64 // 0: no error expected
	}{
		{"a notification is accepted and never answered", `{"jsonrpc":"2.0","method":"notifications/initialized"}`, http.StatusAccepted, 0},
		{"not JSON", `{"jsonrpc":`, http.StatusOK, -32700},
		{"a batch", `[{"jsonrpc":"2.0","id":1,"method":"ping"}]`, http.StatusOK, -32700},
		{"not JSON-RPC 2.0", `{"jsonrpc":"1.0","id":1,"method":"ping"}`, http.StatusOK, -32600},
		{"trailing data", `{"jsonrpc":"2.0","id":1,"method":"ping"} {}`, http.StatusOK, -32700},
		{"tools/call without params", `{"jsonrpc":"2.0","id":1,"method":"tools/call"}`, http.StatusOK, -32602},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := post(t, s, "k", sub, []byte(tc.body))
			if got.status != tc.status {
				t.Fatalf("status = %d, want %d", got.status, tc.status)
			}
			if tc.status == http.StatusAccepted {
				if len(got.body) != 0 {
					t.Fatalf("a notification got a body: %s", got.body)
				}
				return
			}
			if e, _ := got.out["error"].(map[string]any); e["code"] != tc.code {
				t.Fatalf("%s", got.body)
			}
		})
	}
}

func TestABodyOverTheCapIsRefused(t *testing.T) {
	s := server(t, "reviewer")
	body := `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"pad":"` + strings.Repeat("a", maxBody) + `"}}`
	if got := post(t, s, "k", sub, []byte(body)); got.out["error"] == nil {
		t.Fatalf("an oversized body was read: %d %s", got.status, got.body[:min(len(got.body), 200)])
	}
}

func TestOneCallPerSecondPerRun(t *testing.T) {
	c := newClock()
	w := watcher(t, "reviewer")
	w.Upsert(t.Context(), claim("otherrun", "reviewer", "3kq7x2ma", "Running", nil))
	s, rejected := testServer(w, c)
	ok := func(identity string) bool {
		res, _ := call(t, s, identity, "room_read")
		return res["isError"] != true
	}
	if !ok(sub) {
		t.Fatal("the first call is served")
	}
	if ok(sub) {
		t.Fatal("the second call within a second must be refused")
	}
	if !ok("system:serviceaccount:agents:xplane-run-otherrun") {
		t.Fatal("another run has its own budget")
	}
	c.add(999 * time.Millisecond)
	if ok(sub) {
		t.Fatal("999 ms later is still within the second")
	}
	c.add(time.Millisecond)
	if !ok(sub) {
		t.Fatal("a second later the run may call again")
	}
	if strings.Join(*rejected, ",") != "rate_limited,rate_limited" {
		t.Fatalf("rejected = %v", *rejected)
	}
	// tools/list is not a tool call and never spends the budget.
	if out := rpc(t, s, "k", sub, "tools/list", nil); out.out["result"] == nil {
		t.Fatalf("%s", out.body)
	}
	if !func() bool { c.add(time.Second); return ok(sub) }() {
		t.Fatal("listing spent the budget")
	}
}

// The limiter keeps a time per run; a time over a second old is the same as
// none, so the map is swept rather than growing with every run ever seen.
func TestTheLimiterStaysBounded(t *testing.T) {
	c := newClock()
	s := &Server{Now: c.now}
	for i := range maxLimited + 10 {
		if !s.allow(fmt.Sprintf("run%05d", i)) {
			t.Fatalf("run %d refused", i)
		}
		c.add(time.Millisecond)
	}
	if n := len(s.lastCall); n > maxLimited {
		t.Fatalf("%d entries kept, cap %d", n, maxLimited)
	}
	c.add(time.Second)
	s.allow("fresh")
	if n := len(s.lastCall); n > maxLimited {
		t.Fatalf("%d entries kept after a sweep", n)
	}
}

func TestToolErrorsNeverEchoTheLog(t *testing.T) {
	for _, tc := range []struct{ tool, want string }{
		{"room_badargs", "invalid_arguments: text: 1 to 16384 bytes"},
		{"room_sealed", "room_sealed: the room is closed to new events"},
		{"room_broken", "log_unavailable: try again later"},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			c := newClock()
			s, _ := testServer(watcher(t, "reviewer"), c)
			res, text := call(t, s, sub, tc.tool)
			if res["isError"] != true || text != tc.want {
				t.Fatalf("got %q, want %q (%v)", text, tc.want, res)
			}
		})
	}
}

func TestAResultIsTextAndStructured(t *testing.T) {
	s := server(t, "reviewer")
	s.Tools[0].Call = func(context.Context, Caller, json.RawMessage) (any, error) {
		return map[string]any{"lastSeq": 3, "text": "a <b> & c"}, nil
	}
	res, text := call(t, s, sub, "room_read")
	if text != `{"lastSeq":3,"text":"a <b> & c"}` {
		t.Fatalf("text = %q", text)
	}
	if sc, _ := res["structuredContent"].(map[string]any); sc["lastSeq"] != float64(3) {
		t.Fatalf("structuredContent = %v", res["structuredContent"])
	}
}

// The tool sees the run the router verified, its room and role from the
// AgentRun, whatever the arguments claim.
func TestTheCallerIsTheVerifiedRun(t *testing.T) {
	var got Caller
	s := server(t, "reviewer")
	s.Tools = []Tool{{Name: "room_read", Action: policy.Read, Roles: []string{"reviewer"}, InputSchema: objSchema,
		Call: func(_ context.Context, c Caller, _ json.RawMessage) (any, error) {
			got = c
			return map[string]int{}, nil
		}}}
	rpc(t, s, "k", sub, "tools/call", map[string]any{"name": "room_read",
		"arguments": map[string]any{"room": "aaaaaaaa", "role": "implementer"}})
	if got.Run.ID != "7f3cq2xz" || got.Run.Room != "3kq7x2ma" || got.Run.Role != "reviewer" {
		t.Fatalf("caller = %+v", got.Run)
	}
}
