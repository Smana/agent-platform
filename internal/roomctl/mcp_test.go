// SPDX-License-Identifier: Apache-2.0

package roomctl

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/Smana/agent-platform/internal/mcpwire"
)

// mcpBroker is a fake broker /mcp: it records each request and answers by method.
type mcpBroker struct {
	srv      *httptest.Server
	mu       sync.Mutex
	methods  []string
	headers  []http.Header
	polls    int           // tasks/get answers "working" this many times first
	status   int           // when set, every request is refused with it
	draftErr string        // when set, issue_draft refuses with it
	noteWait time.Duration // a notification is recorded this long after it arrives
	// roomText is room_summary's text; its structuredContent is a card for room r1.
	roomText string
	// canStart and canStartWhy are what issue_draft says about starting (Task 12); nil is an older broker.
	canStart    *bool
	canStartWhy string
	// rooms is rooms_list's structuredContent rows; nil answers an empty list.
	rooms []map[string]any
}

func newMCPBroker(t *testing.T) *mcpBroker {
	t.Helper()
	b := &mcpBroker{}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var m message
		_ = json.NewDecoder(r.Body).Decode(&m)
		if len(m.ID) == 0 {
			time.Sleep(b.noteWait)
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		b.methods, b.headers = append(b.methods, m.Method), append(b.headers, r.Header.Clone())
		if b.status != 0 {
			http.Error(w, http.StatusText(b.status), b.status)
			return
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		if len(m.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var res any
		switch m.Method {
		case "initialize":
			res = map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"tools": map[string]any{}}}
		case "tools/list":
			res = map[string]any{"tools": []any{map[string]any{"name": "rooms_list"}}}
		case "tools/call":
			var c struct{ Name string }
			_ = json.Unmarshal(m.Params, &c)
			switch {
			case c.Name == "issue_draft" && b.draftErr != "":
				res = map[string]any{"isError": true, "content": []any{map[string]any{"type": "text", "text": b.draftErr}}}
			case c.Name == "room_summary" && b.roomText != "":
				res = map[string]any{"content": []any{map[string]any{"type": "text", "text": b.roomText}}, "structuredContent": map[string]any{"room": "r1", "url": "https://rooms.example/r/r1"}}
			case c.Name == "issue_draft":
				d := map[string]any{"repo": "Smana/a", "title": "docs: a dead link", "body": "## What is wrong\nstripped"}
				if b.canStart != nil {
					d["canStart"], d["canStartWhy"] = *b.canStart, b.canStartWhy
				}
				res = map[string]any{"content": []any{}, "structuredContent": d}
			case c.Name == "rooms_list" && b.rooms != nil:
				res = map[string]any{"content": []any{map[string]any{"type": "text", "text": "rooms"}}, "structuredContent": map[string]any{"rooms": b.rooms}}
			default:
				res = map[string]any{"content": []any{map[string]any{"type": "text", "text": "{}"}}, "structuredContent": map[string]any{}}
			}
		case "tasks/get":
			status := "completed"
			if b.polls > 0 {
				b.polls--
				status = "working"
			}
			res = map[string]any{"taskId": "tail.x", "status": status, "pollInterval": 5000}
		case "tasks/result":
			res = map[string]any{"content": []any{map[string]any{"type": "text", "text": "page"}}}
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": map[string]any{"code": -32601, "message": "method not found"}})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": res})
	}))
	t.Cleanup(b.srv.Close)
	return b
}

func (b *mcpBroker) seen() ([]string, []http.Header) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.methods...), append([]http.Header(nil), b.headers...)
}

// session is a client on the relay's stdio.
type session struct {
	in    *io.PipeWriter
	lines chan map[string]any
}

// relayTo serves a relay to b, its token from token, set up further by opts.
func relayTo(t *testing.T, b *mcpBroker, token func(context.Context) (string, error), opts ...func(*Relay)) session {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	r := &Relay{URL: b.srv.URL, HC: b.srv.Client(), Token: token,
		After: func(time.Duration) <-chan time.Time { c := make(chan time.Time, 1); c <- time.Now(); return c }}
	for _, o := range opts {
		o(r)
	}
	done := make(chan error, 1)
	go func() { done <- r.Serve(t.Context(), inR, outW); _ = outW.Close() }()
	s := session{in: inW, lines: make(chan map[string]any, 16)}
	go func() {
		sc := bufio.NewScanner(outR)
		for sc.Scan() {
			var m map[string]any
			_ = json.Unmarshal(sc.Bytes(), &m)
			s.lines <- m
		}
		close(s.lines)
	}()
	t.Cleanup(func() {
		_ = inW.Close()
		if err := <-done; err != nil {
			t.Errorf("serve: %v", err)
		}
	})
	return s
}

func tok(context.Context) (string, error) { return "tok", nil }

func (s session) send(t *testing.T, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	if _, err := s.in.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
}

func (s session) next(t *testing.T) map[string]any {
	t.Helper()
	select {
	case m := <-s.lines:
		return m
	case <-time.After(5 * time.Second):
		t.Fatal("no message from the relay")
		return nil
	}
}

// request sends one request and returns its answer.
func (s session) request(t *testing.T, id int, method string, params any) map[string]any {
	t.Helper()
	s.send(t, map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	return s.next(t)
}

// F2: every message reaches the broker's /mcp with the developer's token and, after initialize,
// the revision it settled on; a notification is relayed and never answered.
func TestRelayCarriesTheTokenAndTheVersion(t *testing.T) {
	b := newMCPBroker(t)
	s := relayTo(t, b, tok)
	if res := s.request(t, 1, "initialize", map[string]any{"protocolVersion": "2025-11-25"}); fmt.Sprint(res["result"].(map[string]any)["protocolVersion"]) != "2025-11-25" {
		t.Fatalf("%v", res)
	}
	s.send(t, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	if res := s.request(t, 2, "tools/call", map[string]any{"name": "rooms_list", "arguments": map[string]any{}}); res["id"] != 2.0 || res["result"] == nil {
		t.Fatalf("%v", res)
	}
	methods, headers := b.seen()
	if strings.Join(methods, ",") != "initialize,notifications/initialized,tools/call" {
		t.Fatalf("methods %v", methods)
	}
	if h := headers[2]; h.Get("Authorization") != "Bearer tok" || h.Get("MCP-Protocol-Version") != "2025-11-25" ||
		h.Get("Accept") != "application/json, text/event-stream" {
		t.Fatalf("headers %v", h)
	}
	if res := s.request(t, 3, "resources/list", nil); fmt.Sprint(res["error"]) != "map[code:-32601 message:method not found]" {
		t.Fatalf("the broker's own error, as is: %v", res)
	}
}

// F2: a notification reaches the broker before any later request, however slow the broker is to take it.
func TestRelayPostsNotificationsInOrder(t *testing.T) {
	b := newMCPBroker(t)
	b.noteWait = 50 * time.Millisecond
	s := relayTo(t, b, tok)
	s.request(t, 1, "initialize", map[string]any{"protocolVersion": "2025-11-25"})
	s.send(t, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	s.request(t, 2, "tools/call", map[string]any{"name": "rooms_list", "arguments": map[string]any{}})
	if methods, _ := b.seen(); strings.Join(methods, ",") != "initialize,notifications/initialized,tools/call" {
		t.Fatalf("methods %v", methods)
	}
}

// F5: the relay blocks tasks/result for the client, polling tasks/get first.
func TestRelayBlocksTasksResult(t *testing.T) {
	b := newMCPBroker(t)
	b.polls = 2
	s := relayTo(t, b, tok)
	if res := s.request(t, 1, "tasks/result", map[string]any{"taskId": "tail.x"}); fmt.Sprint(res["result"]) != "map[content:[map[text:page type:text]]]" {
		t.Fatalf("%v", res)
	}
	if methods, _ := b.seen(); strings.Join(methods, ",") != "tasks/get,tasks/get,tasks/get,tasks/result" {
		t.Fatalf("methods %v", methods)
	}
}

func TestRelayExplainsRefusals(t *testing.T) {
	for name, c := range map[string]struct {
		status int
		token  func(context.Context) (string, error)
		want   string
	}{
		"no token":           {0, func(context.Context) (string, error) { return "", errors.New("refresh failed") }, "not_logged_in: run roomctl login"},
		"a refused token":    {http.StatusUnauthorized, tok, "not_logged_in: run roomctl login"},
		"the route is off":   {http.StatusNotFound, tok, "mcp_off"},
		"the broker is down": {http.StatusServiceUnavailable, tok, "mcp: 503"},
	} {
		t.Run(name, func(t *testing.T) {
			b := newMCPBroker(t)
			b.status = c.status
			s := relayTo(t, b, c.token)
			res := s.request(t, 1, "tools/call", map[string]any{"name": "rooms_list"})
			if r := res["result"].(map[string]any); r["isError"] != true || !strings.Contains(fmt.Sprint(r["content"]), c.want) {
				t.Fatalf("a tool call reads it as a tool error: %v", res)
			}
			if res := s.request(t, 2, "ping", nil); !strings.Contains(fmt.Sprint(res["error"]), c.want) {
				t.Fatalf("any other request as a JSON-RPC error: %v", res)
			}
		})
	}
}

// One refresh for a burst of requests: a refresh token the issuer rotates is spent once.
func TestSharedTokenRefreshesOnce(t *testing.T) {
	var mu sync.Mutex
	refreshes, spent := 0, map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		rt := r.Form.Get("refresh_token")
		if spent[rt] {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		spent[rt], refreshes = true, refreshes+1
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "at2", "refresh_token": "rt2", "token_type": "Bearer", "expires_in": 3600})
	}))
	t.Cleanup(srv.Close)
	path := filepath.Join(t.TempDir(), "token.json")
	if err := SaveJSON(path, &oauth2.Token{AccessToken: "old", RefreshToken: "rt", Expiry: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	token := SharedToken(srv.Client(), Config{Issuer: srv.URL, ClientID: "roomctl-client", ProjectID: "1"}, path)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			if got, err := token(t.Context()); err != nil || got != "at2" {
				errs <- fmt.Errorf("%q %w", got, err)
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if refreshes != 1 {
		t.Fatalf("%d refreshes", refreshes)
	}
}

// locals starts a relay whose Relay value the test can read.
func locals(t *testing.T, b *mcpBroker, opts ...func(*Relay)) (session, *Relay) {
	t.Helper()
	var rel *Relay
	return relayTo(t, b, tok, append([]func(*Relay){func(r *Relay) { rel = r }}, opts...)...), rel
}

// The client's elicitation modes (R12): the empty object is form mode, as the spec says.
func TestRelayReadsElicitationModes(t *testing.T) {
	for name, tc := range map[string]struct {
		caps        map[string]any
		forms, urls bool
	}{
		"empty":      {map[string]any{"elicitation": map[string]any{}}, true, false},
		"form":       {map[string]any{"elicitation": map[string]any{"form": map[string]any{}}}, true, false},
		"url":        {map[string]any{"elicitation": map[string]any{"url": map[string]any{}}}, false, true},
		"both":       {map[string]any{"elicitation": map[string]any{"form": map[string]any{}, "url": map[string]any{}}}, true, true},
		"absent":     {map[string]any{}, false, false},
		"null":       {map[string]any{"elicitation": nil}, false, false},
		"with roots": {map[string]any{"roots": map[string]any{}}, false, false},
	} {
		t.Run(name, func(t *testing.T) {
			s, r := locals(t, newMCPBroker(t))
			s.request(t, 1, "initialize", map[string]any{"capabilities": tc.caps})
			r.mu.Lock()
			defer r.mu.Unlock()
			if r.forms != tc.forms || r.urls != tc.urls || r.roots != (name == "with roots") {
				t.Fatalf("forms=%v urls=%v roots=%v", r.forms, r.urls, r.roots)
			}
		})
	}
}

// A local tool blocked on its context ends on the client's cancellation: no reply, and the broker
// never hears of it; the notifications around it are still relayed.
func TestRelayCancelsALocalTool(t *testing.T) {
	b := newMCPBroker(t)
	started, ended := make(chan struct{}), make(chan struct{})
	s, _ := locals(t, b, func(r *Relay) {
		r.handlers = map[string]localTool{toolRoomWait: func(ctx context.Context, _ *Relay, _ localCall) any {
			close(started)
			<-ctx.Done()
			close(ended)
			return mcpwire.ToolError("cancelled")
		}}
	})
	s.send(t, map[string]any{"jsonrpc": "2.0", "id": 7, "method": "tools/call", "params": map[string]any{"name": toolRoomWait}})
	<-started
	s.send(t, map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled", "params": map[string]any{"requestId": 7}})
	<-ended
	s.send(t, map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	if res := s.request(t, 8, "tools/list", nil); res["id"] != 8.0 {
		t.Fatalf("the cancelled call must not be answered: %v", res)
	}
	methods, _ := b.seen()
	if strings.Join(methods, ",") != "notifications/initialized,tools/list" {
		t.Fatalf("the broker saw %v", methods)
	}
}

func TestRelayRendersForAnOlderBroker(t *testing.T) {
	b := newMCPBroker(t)
	b.roomText = `{"room":"r1","url":"https://rooms.example/r/r1"}`
	s := relayTo(t, b, tok)
	res := s.request(t, 1, "tools/call", map[string]any{"name": "room_summary", "arguments": map[string]any{"room": "r1"}})["result"].(map[string]any)
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	if strings.HasPrefix(text, "{") || !strings.Contains(text, "r1") {
		t.Fatalf("the card, not the JSON: %q", text)
	}
	if sc := fmt.Sprint(res["structuredContent"]); sc != "map[room:r1 url:https://rooms.example/r/r1]" {
		t.Fatalf("structuredContent is the broker's: %s", sc)
	}
}

func TestRelayPassesANewBrokersView(t *testing.T) {
	b := newMCPBroker(t)
	b.roomText = "# r1 <b>card</b>\n\n| x |"
	s := relayTo(t, b, tok)
	res := s.request(t, 1, "tools/call", map[string]any{"name": "room_summary", "arguments": map[string]any{"room": "r1"}})["result"].(map[string]any)
	if text := res["content"].([]any)[0].(map[string]any)["text"]; text != b.roomText {
		t.Fatalf("%q", text)
	}
}

func TestRelayListsLocalTools(t *testing.T) {
	s := relayTo(t, newMCPBroker(t), tok)
	var names []string
	for _, tl := range s.request(t, 1, "tools/list", nil)["result"].(map[string]any)["tools"].([]any) {
		names = append(names, tl.(map[string]any)["name"].(string))
	}
	if got := strings.Join(names, ","); got != "rooms_list,issue_file,room_wait,room_checkout" {
		t.Fatalf("%s", got)
	}
}

// Until the tools are wired, they answer unavailable, never a hang.
func TestRelayLocalToolsAreUnavailableUntilWired(t *testing.T) {
	s := relayTo(t, newMCPBroker(t), tok)
	for _, name := range []string{toolRoomWait, toolRoomCheckout} {
		res := s.request(t, 1, "tools/call", map[string]any{"name": name})["result"].(map[string]any)
		if res["isError"] != true || !strings.Contains(fmt.Sprint(res["content"]), "unavailable") {
			t.Fatalf("%s: %v", name, res)
		}
	}
}

func TestRelayProgressNeedsAToken(t *testing.T) {
	var out strings.Builder
	r := &Relay{out: &out}
	r.progress(nil, 1, 2, "no token")
	if out.Len() != 0 {
		t.Fatalf("%q", out.String())
	}
	r.progress(json.RawMessage(`"p1"`), 1, 2, "half")
	if !strings.Contains(out.String(), `"notifications/progress"`) || !strings.Contains(out.String(), `"progressToken":"p1"`) {
		t.Fatalf("%q", out.String())
	}
}

func TestRelayWorkspaceFromRoots(t *testing.T) {
	cwd, _ := os.Getwd()
	t.Run("the first root", func(t *testing.T) {
		s, r := locals(t, newMCPBroker(t))
		s.request(t, 1, "initialize", map[string]any{"capabilities": map[string]any{"roots": map[string]any{}}})
		got := make(chan string, 1)
		go func() { p, _ := r.workspace(t.Context()); got <- p }()
		ask := s.next(t)
		if ask["method"] != "roots/list" {
			t.Fatalf("%v", ask)
		}
		s.send(t, map[string]any{"jsonrpc": "2.0", "id": ask["id"], "result": map[string]any{"roots": []any{map[string]any{"uri": "file:///tmp/x"}}}})
		if p := <-got; p != "/tmp/x" {
			t.Fatalf("%q", p)
		}
	})
	t.Run("a client that never answers", func(t *testing.T) {
		s, r := locals(t, newMCPBroker(t), func(r *Relay) { r.RootsWait = 20 * time.Millisecond })
		s.request(t, 1, "initialize", map[string]any{"capabilities": map[string]any{"roots": map[string]any{}}})
		if p, err := r.workspace(t.Context()); err != nil || p != cwd {
			t.Fatalf("%q %v", p, err)
		}
	})
	t.Run("a client without roots", func(t *testing.T) {
		s, r := locals(t, newMCPBroker(t))
		s.request(t, 1, "initialize", map[string]any{"capabilities": map[string]any{}})
		if p, err := r.workspace(t.Context()); err != nil || p != cwd {
			t.Fatalf("%q %v", p, err)
		}
	})
}

// A local tool reaches the broker as the developer: its text and structuredContent come back whole.
func TestRelayCallsTheBroker(t *testing.T) {
	b := newMCPBroker(t)
	b.roomText = "the card"
	_, r := locals(t, b)
	text, structured, isErr, err := r.callBroker(t.Context(), "room_summary", map[string]any{"room": "r1"})
	if err != nil || isErr || text != "the card" || !strings.Contains(string(structured), `"room":"r1"`) {
		t.Fatalf("%q %s %v %v", text, structured, isErr, err)
	}
}
