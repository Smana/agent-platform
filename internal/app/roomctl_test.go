// SPDX-License-Identifier: Apache-2.0

package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"golang.org/x/oauth2"

	"github.com/Smana/agent-platform/internal/roomctl"
	"github.com/Smana/agent-platform/internal/wire"
)

// roomctlBroker is a fake broker over TLS: it records every act roomctl sends
// and the token it came with, and answers a fork with a new room.
type roomctlBroker struct {
	srv    *httptest.Server
	mu     sync.Mutex
	acts   []map[string]any
	bearer []string
	query  string // the last REST query
	access string // the X-Rooms-Access the room list sends
}

func newRoomctlBroker(t *testing.T) *roomctlBroker {
	t.Helper()
	b := &roomctlBroker{}
	b.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			b.mu.Lock()
			b.query = r.URL.Path + "?" + r.URL.RawQuery
			b.mu.Unlock()
			switch {
			case r.URL.Path == "/api/rooms":
				if b.access != "" {
					w.Header().Set("X-Rooms-Access", b.access)
				}
				_, _ = w.Write([]byte(`[{"id":"3kq7x2ma","repository":"Smana/a","phase":"Active","owner":"human:own","lastSeq":42,"you":{"role":"watcher"}}]`))
			case strings.Contains(r.URL.Path, "/gone/"):
				http.Error(w, "no such room", http.StatusNotFound)
			default:
				_, _ = w.Write([]byte(`{"apiVersion":"summary/v1","room":"26zfnuxm","status":{"phase":"Implementing"},"needsYou":[],"actions":[],` +
					`"notes":{"untrusted":true,"items":[{"at":"2026-10-08T19:14:00Z","run":"cf4ato2x","text":"hi\u001b[2J"}]},"cursor":"seq:142"}`))
			}
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.CloseNow() }()
		var hello wire.ClientFrame
		if wsjson.Read(r.Context(), c, &hello) != nil {
			return
		}
		for {
			var f wire.ClientFrame
			if wsjson.Read(r.Context(), c, &f) != nil {
				return
			}
			var act map[string]any
			_ = json.Unmarshal(f.Action, &act)
			b.mu.Lock()
			b.acts, b.bearer = append(b.acts, act), append(b.bearer, r.Header.Get("Authorization"))
			b.mu.Unlock()
			ack := wire.ServerFrame{Type: wire.FrameAck, ClientSeq: f.ClientSeq, Seq: 43}
			if act["kind"] == "fork" {
				ack.Result = json.RawMessage(`{"roomId":"abcdefgh","run":{"kind":"AgentRun"},"runError":"over_budget"}`)
			}
			_ = wsjson.Write(r.Context(), c, ack)
		}
	}))
	t.Cleanup(b.srv.Close)
	return b
}

// configured is a roomctl directory configured for b, logged in with a token
// valid for an hour.
func configured(t *testing.T, b *roomctlBroker) (roomctlApp Roomctl, out *strings.Builder) {
	t.Helper()
	dir := t.TempDir()
	out = &strings.Builder{}
	r := Roomctl{Dir: dir, HC: b.srv.Client(), Stream: b.srv.Client(), Out: out, Err: &strings.Builder{}}
	if err := r.Run(t.Context(), []string{"configure", "--url", b.srv.URL, "--issuer", "https://auth.example",
		"--client-id", "cli", "--project-id", "29184"}); err != nil {
		t.Fatal(err)
	}
	if err := roomctl.SaveJSON(filepath.Join(dir, "token.json"), &oauth2.Token{AccessToken: "tok-dev", TokenType: "Bearer",
		RefreshToken: "rt", Expiry: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	return r, out
}

func TestRoomctlConfigure(t *testing.T) {
	dir := t.TempDir()
	r := Roomctl{Dir: dir, Out: &strings.Builder{}}
	if err := r.Run(t.Context(), []string{"rooms"}); err == nil || !strings.Contains(err.Error(), "roomctl configure") {
		t.Fatalf("before configure: %v", err)
	}
	if err := r.Run(t.Context(), []string{"configure", "--url", "http://rooms.priv.example", "--issuer", "https://auth.example",
		"--client-id", "cli", "--project-id", "29184"}); err == nil {
		t.Fatal("a plain-http broker would carry the token in clear")
	}
	if err := r.Run(t.Context(), []string{"configure", "--url", "https://rooms.priv.example/", "--issuer", "https://auth.example",
		"--client-id", "cli", "--project-id", "29184"}); err != nil {
		t.Fatal(err)
	}
	var cfg roomctl.Config
	fi, _ := os.Stat(filepath.Join(dir, "config.json"))
	if err := roomctl.LoadJSON(filepath.Join(dir, "config.json"), &cfg); err != nil || fi.Mode().Perm() != 0o600 ||
		cfg != (roomctl.Config{URL: "https://rooms.priv.example", Issuer: "https://auth.example", ClientID: "cli", ProjectID: "29184"}) {
		t.Fatalf("%+v %v %v", cfg, err, fi.Mode().Perm())
	}
	if err := r.Run(t.Context(), []string{"token"}); err == nil || !strings.Contains(err.Error(), "roomctl login") {
		t.Fatalf("before login: %v", err)
	}
}

// Ruling TU: `roomctl token` is the human's own access token, for scripts
// such as task agent:run against the factory's POST /v1/runs.
func TestRoomctlToken(t *testing.T) {
	r, out := configured(t, newRoomctlBroker(t))
	if err := r.Run(t.Context(), []string{"token"}); err != nil || out.String() != "tok-dev\n" {
		t.Fatalf("%q %v", out, err)
	}
}

func TestRoomctlPostAndFork(t *testing.T) {
	b := newRoomctlBroker(t)
	r, out := configured(t, b)
	for _, args := range [][]string{
		{"post", "3kq7x2ma", "--queue", "address", "L42"}, // a flag after the room, as the usage shows it
		{"post", "3kq7x2ma", "looks good"},
		{"fork", "3kq7x2ma", "--at", "12", "--role", "implementer", "--egress", "pypi,npm", "--note", "try uv"},
	} {
		if err := r.Run(t.Context(), args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	want := []string{
		`{"delivery":"queued","kind":"message","text":"address L42"}`,
		`{"delivery":"none","kind":"message","text":"looks good"}`,
		`{"egressProfiles":["pypi","npm"],"kind":"fork","note":"try uv","role":"implementer","seq":12}`,
	}
	for i, w := range want {
		if got, _ := json.Marshal(b.acts[i]); string(got) != w || b.bearer[i] != "Bearer tok-dev" {
			t.Errorf("act %d: %s (%s), want %s", i, got, b.bearer[i], w)
		}
	}
	for _, s := range []string{"seq 43", "forked into abcdefgh: " + b.srv.URL + "/r/abcdefgh", "over your budget", `"kind": "AgentRun"`} {
		if !strings.Contains(out.String(), s) {
			t.Errorf("the output lacks %q:\n%s", s, out)
		}
	}
	for _, bad := range [][]string{{"fork", "3kq7x2ma"}, {"post", "3kq7x2ma"}, {"watch"}, {"fork", "3kq7x2ma", "--at", "0"}} {
		if err := r.Run(t.Context(), bad); err == nil {
			t.Errorf("%v: accepted", bad)
		}
	}
}

// Ruling P18: roomctl has no command that steers, interrupts, moves the driver
// token or decides, and what it sends is a chat, a queued message or a fork.
func TestRoomctlNeverSteersOrDecides(t *testing.T) {
	b := newRoomctlBroker(t)
	r, _ := configured(t, b)
	for _, cmd := range []string{"steer", "interrupt", "promote", "take", "give", "request", "driver", "decide", "approve", "deny", "start", "invite", "close"} {
		if err := r.Run(t.Context(), []string{cmd, "3kq7x2ma", "now"}); err == nil || !strings.Contains(err.Error(), "unknown command") {
			t.Errorf("%s: %v", cmd, err)
		}
	}
	for _, args := range [][]string{{"post", "3kq7x2ma", "--queue", "x"}, {"post", "3kq7x2ma", "x"}, {"fork", "3kq7x2ma", "--at", "1"}} {
		if err := r.Run(t.Context(), args); err != nil {
			t.Fatal(err)
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, a := range b.acts {
		chat := a["kind"] == "message" && slices.Contains([]any{"none", "queued"}, a["delivery"])
		if a["kind"] != "fork" && !chat {
			t.Errorf("roomctl sent %v", a)
		}
	}
}

func TestRoomctlSkillInstall(t *testing.T) {
	skills := filepath.Join(t.TempDir(), "skills")
	out := &strings.Builder{}
	// No configure first: the skill needs neither a config nor a login.
	r := Roomctl{Dir: t.TempDir(), Out: out}
	if err := r.Run(t.Context(), []string{"skill", "install", "--dir", skills}); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"SKILL.md", filepath.Join("references", "issue-template.md")} {
		p := filepath.Join(skills, "factory-handoff", f)
		if !strings.Contains(out.String(), p) {
			t.Errorf("output lacks %s:\n%s", p, out)
		}
		if _, err := os.Stat(p); err != nil {
			t.Error(err)
		}
	}
	if err := r.Run(t.Context(), []string{"skill"}); err == nil {
		t.Error("skill without a subcommand must fail")
	}
}

func TestRoomctlStatus(t *testing.T) {
	b := newRoomctlBroker(t)
	r, out := configured(t, b)
	if err := r.Run(t.Context(), []string{"status", "26zfnuxm", "--after", "5"}); err != nil {
		t.Fatal(err)
	}
	if b.query != "/api/rooms/26zfnuxm/summary?after=5" || !strings.Contains(out.String(), "phase: Implementing") ||
		!strings.Contains(out.String(), "hi[2J") || strings.ContainsRune(out.String(), 0x1b) || !strings.Contains(out.String(), "cursor: seq:142") {
		t.Fatalf("%q\n%q", b.query, out)
	}
	out.Reset()
	if err := r.Run(t.Context(), []string{"status", "--json", "26zfnuxm"}); err != nil || !strings.HasPrefix(out.String(), `{"apiVersion":"summary/v1"`) || !strings.Contains(out.String(), `\u001b`) {
		t.Fatalf("--json is the broker's body: %q %v", out, err)
	}
	if err := r.Run(t.Context(), []string{"status", "gone"}); err == nil || !strings.Contains(err.Error(), "no such room (or you cannot read it)") {
		t.Fatalf("404: %v", err)
	}
	if err := r.Run(t.Context(), []string{"status"}); err == nil {
		t.Fatal("a room is required")
	}
}

// --after takes the summary's cursor as printed, seq:N, or the bare seq.
func TestRoomctlStatusAfterTakesTheCursor(t *testing.T) {
	b := newRoomctlBroker(t)
	r, _ := configured(t, b)
	for _, after := range []string{"seq:142", "142"} {
		if err := r.Run(t.Context(), []string{"status", "26zfnuxm", "--after", after}); err != nil {
			t.Fatal(err)
		}
		if b.query != "/api/rooms/26zfnuxm/summary?after=142" {
			t.Fatalf("--after %s: query %q", after, b.query)
		}
	}
	for _, bad := range []string{"seq:", "seq:-1", "-1", "142x", "cursor:142"} {
		if err := r.Run(t.Context(), []string{"status", "26zfnuxm", "--after", bad}); err == nil {
			t.Errorf("--after %q accepted", bad)
		}
	}
}

// --mine matches the linked GitHub login on the room's task, not the room's owner.
func TestRoomctlHelpSaysWhatMineMatches(t *testing.T) {
	var out strings.Builder
	if err := (Roomctl{Dir: t.TempDir(), Out: &out}).Run(t.Context(), []string{"help"}); err != nil {
		t.Fatal(err)
	}
	if s := out.String(); !strings.Contains(s, "issues you filed or labelled, PRs you authored or review") || strings.Contains(s, "rooms you own") {
		t.Fatalf("help:\n%s", s)
	}
}

func TestRoomctlRoomsFiltersAndHint(t *testing.T) {
	b := newRoomctlBroker(t)
	b.access = "unlinked"
	r, out := configured(t, b)
	if err := r.Run(t.Context(), []string{"rooms", "--repo", "Smana/a", "--mine", "--needs-me"}); err != nil {
		t.Fatal(err)
	}
	if b.query != "/api/rooms?mine=1&needs_me=1&repo=Smana%2Fa" || !strings.Contains(out.String(), "REPO") {
		t.Fatalf("%q %q", b.query, out)
	}
	if got := r.Err.(*strings.Builder).String(); !strings.Contains(got, "https://auth.example") {
		t.Fatalf("the hint goes to stderr and names the issuer: %q", got)
	}
}
