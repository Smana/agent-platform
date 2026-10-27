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
type toolCall struct {
	Name string
	Args map[string]any
}

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
	// roomsFn, when set, answers rooms_list: it gets the call's number (1 first) and its arguments.
	roomsFn func(n int, args map[string]any) []map[string]any
	listed  int
	// calls records each tools/call but rooms_list: the tool and its arguments.
	calls []toolCall
	// card, when set, is room_summary's structuredContent.
	card map[string]any
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
			res = map[string]any{"tools": []any{map[string]any{"name": "rooms_list"},
				map[string]any{"name": "room_summary", "description": "d", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{
					"room": map[string]any{"type": "string", "pattern": "^[a-z2-7]{8}$"}}}}}}
		case "tools/call":
			var c struct {
				Name      string
				Arguments map[string]any
			}
			_ = json.Unmarshal(m.Params, &c)
			if c.Name != "rooms_list" {
				b.calls = append(b.calls, toolCall{c.Name, c.Arguments})
			}
			switch {
			case c.Name == "room_summary" && b.card != nil:
				res = map[string]any{"content": []any{map[string]any{"type": "text", "text": b.roomText}}, "structuredContent": b.card}
			case c.Name == "rooms_list" && b.roomsFn != nil:
				b.listed++
				res = map[string]any{"content": []any{map[string]any{"type": "text", "text": "rooms"}}, "structuredContent": map[string]any{"rooms": b.roomsFn(b.listed, c.Arguments)}}
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
			raw, _ := json.Marshal(tc.caps["elicitation"])
			forms, urls := elicitationModes(raw)
			if r.forms != tc.forms || forms != tc.forms || urls != tc.urls || r.roots != (name == "with roots") {
				t.Fatalf("forms=%v/%v urls=%v roots=%v", r.forms, forms, urls, r.roots)
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
	if got := strings.Join(names, ","); got != "rooms_list,room_summary,issue_file,room_wait,room_checkout" {
		t.Fatalf("%s", got)
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

// blocker is a local tool that blocks until its call is cancelled or released.
type blocker struct {
	mu      sync.Mutex
	started chan int
	release []chan struct{}
	ended   chan int
	hold    map[int]bool // calls that ignore their context until released
}

func newBlocker() *blocker { return &blocker{started: make(chan int, 4), ended: make(chan int, 4)} }

func (b *blocker) tool(ctx context.Context, _ *Relay, _ localCall) any {
	b.mu.Lock()
	n := len(b.release)
	rel := make(chan struct{})
	b.release = append(b.release, rel)
	b.mu.Unlock()
	b.started <- n
	if b.hold[n] {
		<-rel
	} else {
		select {
		case <-ctx.Done():
		case <-rel:
		}
	}
	b.ended <- n
	return mcpwire.ToolError("done")
}

func withBlocker(b *blocker) func(*Relay) {
	return func(r *Relay) { r.handlers = map[string]localTool{toolRoomWait: b.tool} }
}

func callWait(s session, t *testing.T, id any) {
	s.send(t, map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": map[string]any{"name": toolRoomWait}})
}

func cancelOf(s session, t *testing.T, id any) {
	s.send(t, map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled", "params": map[string]any{"requestId": id}})
}

// A cancellation sent right behind the call finds it: the call is registered before it starts.
func TestRelayCancelRightAfterTheCall(t *testing.T) {
	for i := 0; i < 30; i++ {
		b := newBlocker()
		br := newMCPBroker(t)
		s, _ := locals(t, br, withBlocker(b))
		callWait(s, t, 7)
		cancelOf(s, t, 7)
		<-b.ended
		if res := s.request(t, 8, "tools/list", nil); res["id"] != 8.0 {
			t.Fatalf("a cancelled call is not answered: %v", res)
		}
		if methods, _ := br.seen(); strings.Contains(strings.Join(methods, ","), "cancelled") {
			t.Fatalf("the broker heard of a local call's cancellation: %v", methods)
		}
	}
}

// A cancelled call never replies, and a new call that reuses its id keeps its own entry. The first
// call ignores its context, so it is provably still running when the id comes back.
func TestRelayCancelledIdIsReused(t *testing.T) {
	b := newBlocker()
	b.hold = map[int]bool{0: true}
	s, _ := locals(t, newMCPBroker(t), withBlocker(b))
	callWait(s, t, 7)
	<-b.started
	cancelOf(s, t, 7)
	callWait(s, t, 7)
	<-b.started
	cancelOf(s, t, 7) // must cancel the second call, not find the first one's entry
	select {
	case n := <-b.ended:
		if n != 1 {
			t.Fatalf("call %d ended first", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the second call was not cancelled")
	}
	b.mu.Lock()
	close(b.release[0]) // now the first call finishes, after its entry was replaced
	b.mu.Unlock()
	<-b.ended
	if res := s.request(t, 9, "tools/list", nil); res["id"] != 9.0 {
		t.Fatalf("neither cancelled call is answered: %v", res)
	}
}

func TestIDKey(t *testing.T) {
	same := [][]string{{`7`, `7.0`, `7e0`, ` 7 `}, {`"7"`}, {`null`}, {`"a"`}}
	for i, g := range same {
		for _, a := range g {
			for _, b := range g {
				if idKey(json.RawMessage(a)) != idKey(json.RawMessage(b)) {
					t.Errorf("%s and %s are the same id", a, b)
				}
			}
			for _, other := range same[i+1:] {
				if idKey(json.RawMessage(a)) == idKey(json.RawMessage(other[0])) {
					t.Errorf("%s and %s are not the same id", a, other[0])
				}
			}
		}
	}
}

// A call that finishes after its cancellation is never answered, for any local tool.
func TestRelayNoReplyToACancelledLocalToolThatFinishesLater(t *testing.T) {
	for _, name := range []string{toolRoomCheckout, issueFileTool} {
		t.Run(name, func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			s, _ := locals(t, newMCPBroker(t), func(r *Relay) {
				r.handlers = map[string]localTool{name: func(context.Context, *Relay, localCall) any {
					close(started)
					<-release // ignores its context: it completes after the cancel
					return mcpwire.ToolError("late")
				}}
			})
			s.send(t, map[string]any{"jsonrpc": "2.0", "id": 7, "method": "tools/call", "params": map[string]any{"name": name}})
			<-started
			cancelOf(s, t, 7)
			if res := s.request(t, 8, "tools/list", nil); res["id"] != 8.0 { // the cancel was read before this
				t.Fatalf("%v", res)
			}
			close(release)
			// Serve returns only after the call has: closing stdin is the sync point.
			_ = s.in.Close()
			for m := range s.lines {
				t.Fatalf("the late result of a cancelled call was written: %v", m)
			}
		})
	}
}

// Ids compare as JSON values: 7 is 7.0, and the string "7" is another id.
func TestRelayCancelNormalisesTheId(t *testing.T) {
	b := newBlocker()
	s, _ := locals(t, newMCPBroker(t), withBlocker(b))
	callWait(s, t, 7)
	<-b.started
	cancelOf(s, t, "7")
	select {
	case <-b.ended:
		t.Fatal(`the string "7" cancelled the call 7`)
	case <-time.After(100 * time.Millisecond):
	}
	cancelOf(s, t, 7.0)
	<-b.ended
	if res := s.request(t, 9, "tools/list", nil); res["id"] != 9.0 {
		t.Fatalf("%v", res)
	}
}

// viewed renders only a payload it can trust, and touches nothing else.
func TestViewedKeepsWhatItCannotRender(t *testing.T) {
	mk := func(isErr bool, sc string, extra string) json.RawMessage {
		s := `{"content":[{"type":"text","text":"{\"room\":\"r1\"}"}` + extra + `]`
		if sc != "" {
			s += `,"structuredContent":` + sc
		}
		if isErr {
			s += `,"isError":true`
		}
		return json.RawMessage(s + `}`)
	}
	for name, tc := range map[string]struct {
		tool string
		res  json.RawMessage
		same bool
	}{
		"null":        {"room_summary", mk(false, "null", ""), true},
		"absent":      {"room_summary", mk(false, "", ""), true},
		"empty":       {"room_summary", mk(false, "{}", ""), true},
		"wrong shape": {"room_summary", mk(false, `[1]`, ""), true},
		"list no key": {"rooms_list", mk(false, `{"x":1}`, ""), true},
		"is error":    {"room_summary", mk(true, `{"room":"r1"}`, ""), true},
		"renders":     {"room_summary", mk(false, `{"room":"r1"}`, ""), false},
	} {
		t.Run(name, func(t *testing.T) {
			got := viewed(tc.tool, tc.res, "https://rooms.example", time.Now())
			if (string(got) == string(tc.res)) != tc.same {
				t.Fatalf("same=%v: %s", tc.same, got)
			}
		})
	}
	t.Run("keeps the other content", func(t *testing.T) {
		got := viewed("room_summary", mk(false, `{"room":"r1"}`, `,{"type":"text","text":"second"}`), "", time.Now())
		if !strings.Contains(string(got), `"second"`) {
			t.Fatalf("%s", got)
		}
	})
}

func TestRelayProgressOmitsAnUnknownTotal(t *testing.T) {
	var out strings.Builder
	r := &Relay{out: &out}
	r.progress(json.RawMessage(`7`), 1, 0, "")
	if strings.Contains(out.String(), "total") {
		t.Fatalf("%q", out.String())
	}
}

func TestRelayCheckoutToolIsDescribed(t *testing.T) {
	s := relayTo(t, newMCPBroker(t), tok)
	for _, tl := range s.request(t, 1, "tools/list", nil)["result"].(map[string]any)["tools"].([]any) {
		m := tl.(map[string]any)
		if m["name"] == toolRoomCheckout && (strings.Contains(m["description"].(string), "Not yet") || !strings.Contains(fmt.Sprint(m["inputSchema"]), "required")) {
			t.Fatalf("%v", m)
		}
	}
}
