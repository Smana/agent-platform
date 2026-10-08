// SPDX-License-Identifier: Apache-2.0

package roomctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"golang.org/x/oauth2"

	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/wire"
)

func TestLineIsOneReadableRow(t *testing.T) {
	agent := envelope.Actor{Kind: envelope.ActorAgent, ID: "agent:7f3cq2xz", Role: "reviewer"}
	cases := []struct {
		name string
		ev   envelope.Event
		want string
	}{
		{"a verdict", envelope.Event{Seq: 12, Actor: agent, Type: envelope.Message,
			Payload: envelope.Must(envelope.MessagePayload{Kind: envelope.KindReviewVerdict, Verdict: "changes", Text: "Add a test.\nAnd docs."})},
			"#12 agent:7f3cq2xz (reviewer) review_verdict changes: Add a test. And docs."},
		{"a queued message", envelope.Event{Seq: 3, Actor: envelope.Actor{Kind: envelope.ActorHuman, ID: "human:291"}, Type: envelope.Message,
			Payload: envelope.Must(envelope.MessagePayload{Kind: envelope.KindChat, Text: "address L42", Delivery: envelope.DeliveryQueued})},
			"#3 human:291 message queued: address L42"},
		{"a tool call", envelope.Event{Seq: 4, Actor: agent, Type: envelope.ToolCall,
			Payload: envelope.Must(map[string]any{"tool": "terminal", "args": map[string]any{"command": "go test ./..."}})},
			"#4 agent:7f3cq2xz (reviewer) tool_call terminal: go test ./..."},
		{"a state change", envelope.Event{Seq: 5, Actor: envelope.Actor{Kind: envelope.ActorSystem, ID: "system:room-broker"},
			Type: envelope.StateChanged, Payload: envelope.StatePayload("run_phase", map[string]any{"phase": "Running"})},
			"#5 system:room-broker run_phase Running"},
		{"a handoff", envelope.Event{Seq: 6, Actor: agent, Type: envelope.Handoff,
			Payload: envelope.Must(envelope.HandoffPayload{FromRole: "implementer", ToRole: "reviewer", Commit: "4be1c9d", Summary: "Opened #12"})},
			"#6 agent:7f3cq2xz (reviewer) handoff → reviewer @ 4be1c9d: Opened #12"},
		// T10's terminal twin: room text is untrusted, and an escape sequence
		// would drive the developer's terminal (OSC 52 writes the clipboard).
		{"no escape reaches the terminal", envelope.Event{Seq: 7, Actor: agent, Type: envelope.Message,
			Payload: envelope.Must(envelope.MessagePayload{Kind: envelope.KindChat, Text: "hi\x1b]52;c;cm0gLXJmIH4=\x07 \x1b[2Jthere\u202e"})},
			"#7 agent:7f3cq2xz (reviewer) message: hi]52;c;cm0gLXJmIH4= [2Jthere"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Line(c.ev); got != c.want {
				t.Fatalf("%q", got)
			}
		})
	}
	long := envelope.Event{Seq: 8, Actor: agent, Type: envelope.Message,
		Payload: envelope.Must(envelope.MessagePayload{Kind: envelope.KindChat, Text: strings.Repeat("é", 300)})}
	if got := Line(long); !strings.HasSuffix(got, "…") || len([]rune(got)) > 200 {
		t.Fatalf("not clipped on a rune: %q", got)
	}
}

func TestSecretsAreOwnerOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "roomctl")
	path := filepath.Join(dir, "token.json")
	if err := SaveJSON(path, map[string]string{"access_token": "x"}); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
	if fi, _ := os.Stat(dir); fi.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", fi.Mode().Perm())
	}
	// A file a looser umask or an older version left readable is tightened.
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := SaveJSON(path, map[string]string{"access_token": "y"}); err != nil {
		t.Fatal(err)
	}
	var got map[string]string
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 || LoadJSON(path, &got) != nil || got["access_token"] != "y" {
		t.Fatalf("mode %v, %v", fi.Mode().Perm(), got)
	}
}

func TestConfigIsHTTPSAndComplete(t *testing.T) {
	ok := Config{URL: "https://rooms.priv.example", Issuer: "https://auth.example", ClientID: "c", ProjectID: "p"}
	if err := ok.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, bad := range map[string]func(*Config){
		"a plain-http broker": func(c *Config) { c.URL = "http://rooms.priv.example" },
		"a plain-http issuer": func(c *Config) { c.Issuer = "http://auth.example" },
		"no host":             func(c *Config) { c.URL = "https://" },
		"no client":           func(c *Config) { c.ClientID = "" },
		"no project":          func(c *Config) { c.ProjectID = "" },
	} {
		c := ok
		bad(&c)
		if c.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// issuer is a ZITADEL-shaped fake: the device authorization grant, then the
// token endpoint, which answers authorization_pending once.
func issuer(t *testing.T) (*httptest.Server, *[]string) {
	t.Helper()
	var mu sync.Mutex
	var forms []string
	pending := true
	mux := http.NewServeMux()
	mux.HandleFunc("POST /oauth/v2/device_authorization", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		forms = append(forms, r.Form.Encode())
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"device_code": "dc", "user_code": "ABCD-EFGH",
			"verification_uri": "https://auth.example/device", "expires_in": 300, "interval": 1})
	})
	mux.HandleFunc("POST /oauth/v2/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		mu.Lock()
		defer mu.Unlock()
		if _, _, basic := r.BasicAuth(); basic {
			w.WriteHeader(http.StatusUnauthorized) // a native app has no secret to send
			return
		}
		forms = append(forms, r.Form.Encode())
		w.Header().Set("Content-Type", "application/json")
		if r.Form.Get("grant_type") == "urn:ietf:params:oauth:grant-type:device_code" && pending {
			pending = false
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"authorization_pending"}`))
			return
		}
		at := "at"
		if r.Form.Get("grant_type") == "refresh_token" {
			at = "at2"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": at, "token_type": "Bearer", "expires_in": 3600, "refresh_token": "rt"})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, &forms
}

// The device authorization grant (§8): no redirect to this machine, no secret,
// and the project's audience scope, without which the broker refuses the token.
func TestLoginUsesTheDeviceFlow(t *testing.T) {
	srv, forms := issuer(t)
	var prompt strings.Builder
	c := Config{Issuer: srv.URL, ClientID: "roomctl-client", ProjectID: "29184"}
	tok, err := Login(t.Context(), srv.Client(), c, &prompt)
	if err != nil || tok.AccessToken != "at" || tok.RefreshToken != "rt" || !strings.Contains(prompt.String(), "ABCD-EFGH") ||
		!strings.Contains(prompt.String(), "https://auth.example/device") {
		t.Fatal(tok, err, prompt.String())
	}
	asked := (*forms)[0]
	for _, want := range []string{"client_id=roomctl-client", "offline_access", "urn%3Azitadel%3Aiam%3Aorg%3Aproject%3Aid%3A29184%3Aaud"} {
		if !strings.Contains(asked, want) {
			t.Errorf("the device authorization lacks %s: %s", want, asked)
		}
	}
	if last := (*forms)[len(*forms)-1]; !strings.Contains(last, "device_code=dc") || strings.Contains(last, "client_secret") {
		t.Errorf("token request %s", last)
	}
}

func TestTokenRefreshesAndSaves(t *testing.T) {
	srv, forms := issuer(t)
	c := Config{Issuer: srv.URL, ClientID: "roomctl-client", ProjectID: "29184"}
	path := filepath.Join(t.TempDir(), "token.json")
	if _, err := Token(t.Context(), srv.Client(), c, path); err == nil || !strings.Contains(err.Error(), "roomctl login") {
		t.Fatalf("no token yet: %v", err)
	}
	if err := SaveJSON(path, &oauth2.Token{AccessToken: "old", RefreshToken: "rt", Expiry: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if got, err := Token(t.Context(), srv.Client(), c, path); err != nil || got != "at2" {
		t.Fatalf("refresh: %q %v", got, err)
	}
	var saved oauth2.Token
	if err := LoadJSON(path, &saved); err != nil || saved.AccessToken != "at2" || saved.RefreshToken != "rt" {
		t.Fatalf("saved %+v %v", saved, err)
	}
	calls := len(*forms)
	if got, err := Token(t.Context(), srv.Client(), c, path); err != nil || got != "at2" || len(*forms) != calls {
		t.Fatalf("a valid token is used as is: %q %v, %d calls", got, err, len(*forms)-calls)
	}
}

// broker is a fake room broker over TLS: the room list, and a WebSocket that
// records hellos and acts, serves events, and answers each act.
type broker struct {
	srv    *httptest.Server
	mu     sync.Mutex
	hellos []wire.ClientFrame
	acts   []json.RawMessage
	tokens []string
	query  []string // the raw queries of the room list and the summary
	access string   // the X-Rooms-Access value the room list sends
	// conn serves the n-th connection after its hello; nil serves state, then acks.
	conn func(n int, ctx context.Context, c *websocket.Conn)
}

func newBroker(t *testing.T) *broker {
	t.Helper()
	b := &broker{}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/rooms", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		b.mu.Lock()
		b.query = append(b.query, r.URL.RawQuery)
		access := b.access
		b.mu.Unlock()
		if access != "" {
			w.Header().Set("X-Rooms-Access", access)
		}
		_, _ = w.Write([]byte(`[{"id":"3kq7x2ma","repository":"Smana/a","needsMe":true,"phase":"Active","owner":"human:own","driver":"human:own","dataClass":"public",` +
			`"lastSeq":42,"you":{"principal":"human:dev","role":"watcher","approver":false,"driver":false,"webUI":false}}]`))
	})
	mux.HandleFunc("GET /api/rooms/{id}/summary", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		b.query = append(b.query, r.URL.RawQuery)
		b.mu.Unlock()
		switch r.PathValue("id") {
		case "gone":
			http.Error(w, "no such room", http.StatusNotFound)
		case "down":
			http.Error(w, "room log unreadable", http.StatusServiceUnavailable)
		default:
			_, _ = w.Write([]byte(summaryBody))
		}
	})
	mux.HandleFunc("GET /v1/ws", func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		b.tokens = append(b.tokens, r.Header.Get("Authorization"))
		b.mu.Unlock()
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.CloseNow() }()
		var hello wire.ClientFrame
		if wsjson.Read(r.Context(), c, &hello) != nil {
			return
		}
		b.mu.Lock()
		b.hellos = append(b.hellos, hello)
		n := len(b.hellos)
		b.mu.Unlock()
		if b.conn != nil {
			b.conn(n, r.Context(), c)
			return
		}
		_ = wsjson.Write(r.Context(), c, wire.ServerFrame{Type: wire.FrameState, ThroughSeq: 42,
			Snapshot: &wire.Snapshot{RoomID: hello.RoomID, Phase: "Active", You: wire.You{Role: "watcher"}}})
		for {
			var f wire.ClientFrame
			if wsjson.Read(r.Context(), c, &f) != nil {
				return
			}
			b.mu.Lock()
			b.acts = append(b.acts, f.Action)
			b.mu.Unlock()
			_ = wsjson.Write(r.Context(), c, wire.ServerFrame{Type: wire.FrameAck, ClientSeq: f.ClientSeq, Seq: 43,
				Result: json.RawMessage(`{"roomId":"abcdefgh"}`)})
		}
	})
	b.srv = httptest.NewTLSServer(mux)
	t.Cleanup(b.srv.Close)
	return b
}

func (b *broker) client(token func(context.Context) (string, error)) Client {
	return Client{URL: b.srv.URL, Token: token, HC: b.srv.Client(), Stream: b.srv.Client(), Backoff: time.Millisecond}
}

func fixed(context.Context) (string, error) { return "tok", nil }

func TestRoomsListsWhatTheCallerReads(t *testing.T) {
	b := newBroker(t)
	var out strings.Builder
	if err := b.client(fixed).Rooms(t.Context(), &out, RoomFilter{}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 || !slices.Equal(strings.Fields(lines[0]), []string{"ROOM", "REPO", "PHASE", "EVENTS", "OWNER", "YOU"}) ||
		!slices.Equal(strings.Fields(lines[1]), []string{"3kq7x2ma", "Smana/a", "Active", "42", "human:own", "watcher"}) {
		t.Fatalf("%q", lines)
	}
	bad := b.client(func(context.Context) (string, error) { return "nope", nil })
	if err := bad.Rooms(t.Context(), &out, RoomFilter{}); err == nil || !strings.Contains(err.Error(), "roomctl login") {
		t.Fatalf("a refused token: %v", err)
	}
}

// One act, on a connection of its own that replays nothing: hello asks from past the mark.
func TestActSendsOneActionAndReturnsItsAck(t *testing.T) {
	b := newBroker(t)
	f, err := b.client(fixed).Act(t.Context(), "3kq7x2ma", map[string]any{"kind": "message", "delivery": "queued", "text": "address L42"})
	if err != nil || f.Type != wire.FrameAck || f.Seq != 43 || string(f.Result) != `{"roomId":"abcdefgh"}` {
		t.Fatalf("%+v %v", f, err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if h := b.hellos[0]; h.Type != wire.FrameHello || h.RoomID != "3kq7x2ma" || h.AfterSeq == nil || *h.AfterSeq != math.MaxInt64 {
		t.Fatalf("hello %+v", h)
	}
	if string(b.acts[0]) != `{"delivery":"queued","kind":"message","text":"address L42"}` || b.tokens[0] != "Bearer tok" {
		t.Fatalf("act %s, token %q", b.acts[0], b.tokens[0])
	}
}

// Watch follows the room across the broker's reconnect closes (docs/api.md):
// with a fresh token, from the last seq it printed, never printing one twice.
func TestWatchFollowsAcrossReauth(t *testing.T) {
	b := newBroker(t)
	ev := func(seq int64) wire.ServerFrame {
		return wire.ServerFrame{Type: wire.FrameEvent, Event: &envelope.Event{Seq: seq, Actor: envelope.Actor{Kind: envelope.ActorHuman, ID: "human:a"},
			Type: envelope.Message, Payload: envelope.Must(envelope.MessagePayload{Kind: envelope.KindChat, Text: "m"})}}
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	b.conn = func(n int, ctx context.Context, c *websocket.Conn) {
		_ = wsjson.Write(ctx, c, wire.ServerFrame{Type: wire.FrameState, ThroughSeq: 2,
			Snapshot: &wire.Snapshot{RoomID: "3kq7x2ma", Phase: "Active", Driver: "human:own", You: wire.You{Role: "watcher"}}})
		switch n {
		case 1:
			_ = wsjson.Write(ctx, c, ev(1))
			_ = wsjson.Write(ctx, c, ev(2))
			_ = c.Close(4001, "reauth")
		case 2:
			_ = wsjson.Write(ctx, c, ev(2)) // the clamp resends the mark
			_ = wsjson.Write(ctx, c, ev(3))
			<-ctx.Done()
		}
	}
	var calls atomic.Int32
	token := func(context.Context) (string, error) { return fmt.Sprintf("tok%d", calls.Add(1)), nil }
	out := &until{stop: "#3 ", cancel: cancel}
	if err := b.client(token).Watch(ctx, "3kq7x2ma", 50, out); err != nil {
		t.Fatalf("a watch ended by its context is not an error: %v", err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.hellos) != 2 || b.hellos[0].Tail != 50 || b.hellos[1].AfterSeq == nil || *b.hellos[1].AfterSeq != 2 ||
		b.tokens[0] != "Bearer tok1" || b.tokens[1] != "Bearer tok2" {
		t.Fatalf("hellos %+v, tokens %v", b.hellos, b.tokens)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 4 || !strings.HasPrefix(lines[0], "# room 3kq7x2ma") || !strings.Contains(lines[0], "you: watcher") ||
		!strings.HasPrefix(lines[1], "#1 ") || !strings.HasPrefix(lines[2], "#2 ") || !strings.HasPrefix(lines[3], "#3 ") {
		t.Fatalf("%q", lines)
	}
}

// until is the watch's output; it ends the watch once a line starts with stop.
type until struct {
	strings.Builder
	stop   string
	cancel func()
}

func (u *until) Write(p []byte) (int, error) {
	n, err := u.Builder.Write(p)
	if strings.HasPrefix(string(p), u.stop) {
		u.cancel()
	}
	return n, err
}

// A refusal before the upgrade is not retried: it needs the developer.
func TestWatchStopsOnARefusal(t *testing.T) {
	var dials atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		dials.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()
	c := Client{URL: srv.URL, Token: fixed, HC: srv.Client(), Stream: srv.Client(), Backoff: time.Millisecond}
	err := c.Watch(t.Context(), "3kq7x2ma", 50, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "403") || dials.Load() != 1 {
		t.Fatalf("%v after %d dials", err, dials.Load())
	}
}

// A dial that cannot get a token, or a frame over the read limit, would fail the
// same way on every reconnect: the watch ends with why instead of retrying.
func TestWatchStopsWhereRetryingCannotHelp(t *testing.T) {
	state := wire.ServerFrame{Type: wire.FrameState, ThroughSeq: 2,
		Snapshot: &wire.Snapshot{RoomID: "3kq7x2ma", Phase: "Active", You: wire.You{Role: "watcher"}}}
	cases := []struct {
		name  string
		token func(context.Context) (string, error)
		conn  func(ctx context.Context, c *websocket.Conn)
		want  string
	}{
		{"a refresh the issuer refuses", func() func(context.Context) (string, error) {
			var calls atomic.Int32
			return func(context.Context) (string, error) {
				if calls.Add(1) == 1 {
					return "tok", nil
				}
				return "", errors.New(`refresh the token (oauth2: "invalid_grant"): run roomctl login`)
			}
		}(), func(ctx context.Context, c *websocket.Conn) {
			_ = wsjson.Write(ctx, c, state)
			_ = c.Close(4001, "reauth")
		}, "run roomctl login"},
		{"a frame over the read limit", fixed, func(ctx context.Context, c *websocket.Conn) {
			_ = wsjson.Write(ctx, c, state)
			_ = c.Write(ctx, websocket.MessageText, []byte(`"`+strings.Repeat("x", maxFrame)+`"`))
			<-ctx.Done()
		}, "4 MiB"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newBroker(t)
			b.conn = func(_ int, ctx context.Context, c *websocket.Conn) { tc.conn(ctx, c) }
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			err := b.client(tc.token).Watch(ctx, "3kq7x2ma", 50, &strings.Builder{})
			b.mu.Lock()
			defer b.mu.Unlock()
			if err == nil || !strings.Contains(err.Error(), tc.want) || len(b.hellos) != 1 {
				t.Fatalf("%v after %d dials", err, len(b.hellos))
			}
		})
	}
}

const summaryBody = `{"apiVersion":"summary/v1","room":"26zfnuxm","url":"https://rooms.example/r/26zfnuxm",` +
	`"status":{"phase":"Implementing","run":{"id":"cf4ato2x","role":"implementer"},"budget":{"usedTokens":189093,"limitTokens":1500000},` +
	`"pr":{"number":2239,"url":"https://github.com/Smana/a/pull/2239"},"issue":null,"lastVerdict":{"by":"reviewer","verdict":"changes_requested\u001b[31m","at":"2026-10-08T19:00:00Z"}},` +
	`"needsYou":[{"kind":"approval","id":"01M4","what":"git push to agent/26zfnuxm","deadline":"2026-10-08T14:00:00Z","url":"https://rooms.example/r/26zfnuxm#01M4"}],` +
	`"actions":[{"kind":"queue","what":"queue a message","cli":"roomctl post 26zfnuxm --queue <text>"},{"kind":"steer","what":"steer the run"}],` +
	`"notes":{"untrusted":true,"items":[{"at":"2026-10-08T19:14:00Z","run":"cf4ato2x","text":"found both versions\u001b[2J on line 12-13, fixing"}]},"cursor":"seq:142"}`

func TestSummaryIsTheBrokersBodyUnchanged(t *testing.T) {
	b := newBroker(t)
	raw, err := b.client(fixed).Summary(t.Context(), "26zfnuxm", 7)
	if err != nil || string(raw) != summaryBody {
		t.Fatalf("%s %v", raw, err)
	}
	if got := b.query[len(b.query)-1]; got != "after=7" {
		t.Fatalf("query %q", got)
	}
}

func TestSummaryRefusals(t *testing.T) {
	b := newBroker(t)
	for room, want := range map[string]string{"gone": "no such room (or you cannot read it)", "down": "room unavailable"} {
		if _, err := b.client(fixed).Summary(t.Context(), room, 0); err == nil || !strings.Contains(err.Error(), want) || strings.Contains(err.Error(), "\n") {
			t.Fatalf("%s: %v", room, err)
		}
	}
}

func TestRenderSummary(t *testing.T) {
	var out strings.Builder
	if err := RenderSummary(&out, []byte(summaryBody), time.Date(2026, 10, 8, 19, 30, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	want := `phase: Implementing  run: cf4ato2x (implementer)  budget: 189093/1500000
PR #2239 https://github.com/Smana/a/pull/2239
last verdict: reviewer changes_requested[31m
needs you: approve "git push to agent/26zfnuxm" by 14:00 UTC → https://rooms.example/r/26zfnuxm#01M4
actions:
  queue a message: roomctl post 26zfnuxm --queue <text>
  steer the run
notes (the agents' claims):
  19:14 UTC cf4ato2x  found both versions[2J on line 12-13, fixing
cursor: seq:142
`
	if out.String() != want {
		t.Fatalf("got:\n%s\nwant:\n%s", out.String(), want)
	}
	if strings.ContainsRune(out.String(), 0x1b) || strings.Contains(out.String(), "roomctl approve") {
		t.Fatal("escape or approve command")
	}
}

// A time not of today (UTC) carries its date: "by 14:00" read the next morning is a deadline long gone.
func TestRenderSummaryDatesAnotherDay(t *testing.T) {
	var out strings.Builder
	if err := RenderSummary(&out, []byte(summaryBody), time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"by 2026-10-08T14:00:00Z →", "  2026-10-08T19:14:00Z cf4ato2x"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("no %q in:\n%s", want, out.String())
		}
	}
}

func TestRoomsFilterAndAccessHint(t *testing.T) {
	b := newBroker(t)
	c := b.client(fixed)
	c.Issuer = "https://auth.example"
	for _, tc := range []struct{ access, hint string }{
		{"ok", ""}, {"", ""},
		{"unlinked", "no linked GitHub identity"},
		{"unverified", "could not be verified"},
	} {
		b.access = tc.access
		var out, errOut strings.Builder
		c.Err = &errOut
		if err := c.Rooms(t.Context(), &out, RoomFilter{Repo: "Smana/a", NeedsMe: true}); err != nil {
			t.Fatal(err)
		}
		if got := b.query[len(b.query)-1]; got != "needs_me=1&repo=Smana%2Fa" {
			t.Fatalf("query %q", got)
		}
		if !strings.Contains(out.String(), "REPO") || strings.Contains(out.String(), "verified") {
			t.Fatalf("stdout %q", out.String())
		}
		if tc.hint == "" && errOut.Len() != 0 || !strings.Contains(errOut.String(), tc.hint) {
			t.Fatalf("%q: stderr %q", tc.access, errOut.String())
		}
		if tc.access == "unlinked" && !strings.Contains(errOut.String(), "https://auth.example") {
			t.Fatalf("unlinked names the issuer: %q", errOut.String())
		}
		// Unlinked is never an admin (admins bypass D7): they see no room at all.
		if tc.access == "unlinked" && (!strings.Contains(errOut.String(), "no room is listed") || strings.Contains(errOut.String(), "admins")) {
			t.Fatalf("unlinked says what they see: %q", errOut.String())
		}
		if tc.access == "unverified" && strings.Contains(errOut.String(), "retry") {
			t.Fatalf("unverified promises a retry: %q", errOut.String())
		}
	}
}
