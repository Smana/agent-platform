// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/Smana/agent-platform/internal/roomctl"
	"github.com/Smana/agent-platform/internal/wire"
)

// #48: `configure --url` alone reads the rest from the broker, so configure and login are the
// whole onboarding.
func TestRoomctlConfigureDiscoversTheRest(t *testing.T) {
	b := newRoomctlBroker(t)
	dir := t.TempDir()
	out := &strings.Builder{}
	r := Roomctl{Dir: dir, HC: b.srv.Client(), Stream: b.srv.Client(), Out: out, Err: &strings.Builder{}}
	if err := r.Run(t.Context(), []string{"configure", "--url", b.srv.URL + "/"}); err != nil {
		t.Fatal(err)
	}
	var cfg roomctl.Config
	if err := roomctl.LoadJSON(filepath.Join(dir, "config.json"), &cfg); err != nil ||
		cfg != (roomctl.Config{URL: b.srv.URL, Issuer: "https://auth.example", ClientID: "cli", ProjectID: "29184"}) {
		t.Fatalf("%+v %v", cfg, err)
	}
	if got := r.Err.(*strings.Builder).String(); !strings.Contains(got, "roomctl login") || out.Len() != 0 {
		t.Fatalf("configure says what comes next on the hint stream: %q, stdout %q", got, out)
	}
	// A flag still wins over what the broker says.
	if err := r.Run(t.Context(), []string{"configure", "--url", b.srv.URL, "--client-id", "other"}); err != nil {
		t.Fatal(err)
	}
	if err := roomctl.LoadJSON(filepath.Join(dir, "config.json"), &cfg); err != nil || cfg.ClientID != "other" || cfg.ProjectID != "29184" {
		t.Fatalf("%+v %v", cfg, err)
	}
}

// A refused connection or an untrusted CA names its cause: Run is the one place every command's
// error passes.
func TestRoomctlErrorsNameTheCause(t *testing.T) {
	b := newRoomctlBroker(t)
	r, _ := configured(t, b)
	r.HC = &http.Client{} // the default pool does not hold the test CA
	err := r.Run(t.Context(), []string{"rooms"})
	if err == nil || !strings.Contains(err.Error(), "trust the platform CA") {
		t.Fatalf("%v", err)
	}
	if err := r.Run(t.Context(), []string{"configure", "--url", b.srv.URL}); err == nil || !strings.Contains(err.Error(), "trust the platform CA") {
		t.Fatalf("discovery: %v", err)
	}
}

func TestRoomctlDoctorRunsTheChecks(t *testing.T) {
	// doctor reads the skill directories under the home directory: never the machine's own.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	b := newRoomctlBroker(t)
	r, out := configured(t, b)
	if err := r.Run(t.Context(), []string{"doctor"}); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(out.String(), "github") || strings.Contains(out.String(), "FAIL") {
		t.Fatalf("%s", out)
	}
	b.access = "unlinked"
	out.Reset()
	if err := r.Run(t.Context(), []string{"doctor"}); err == nil || !strings.Contains(out.String(), "ask an admin to link") {
		t.Fatalf("%v\n%s", err, out)
	}
	fresh := Roomctl{Dir: t.TempDir(), Out: &strings.Builder{}}
	if err := fresh.Run(t.Context(), []string{"doctor"}); err == nil || !strings.Contains(err.Error(), "roomctl configure --url") {
		t.Fatalf("not configured: %v", err)
	}
}

func TestRoomctlRoomsJSONIsTheBrokersBody(t *testing.T) {
	b := newRoomctlBroker(t)
	r, out := configured(t, b)
	if err := r.Run(t.Context(), []string{"rooms", "--json"}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), `[{"id":"3kq7x2ma"`) {
		t.Fatalf("%s", out)
	}
}

// O3a: `configure --from <rooms URL>` takes the four values from the broker, whatever the stored
// config says, so a rebuilt platform's new client id replaces the stale one. A rooms page URL is
// reduced to its origin; a plain-http URL is refused.
func TestRoomctlConfigureFrom(t *testing.T) {
	b := newRoomctlBroker(t)
	dir := t.TempDir()
	r := Roomctl{Dir: dir, HC: b.srv.Client(), Stream: b.srv.Client(), Out: &strings.Builder{}, Err: &strings.Builder{}}
	stale := roomctl.Config{URL: b.srv.URL, Issuer: "https://old.example", ClientID: "stale", ProjectID: "1"}
	if err := roomctl.SaveJSON(filepath.Join(dir, "config.json"), stale); err != nil {
		t.Fatal(err)
	}
	if err := r.Run(t.Context(), []string{"configure", "--from", b.srv.URL + "/r/26zfnuxm", "--yes"}); err != nil {
		t.Fatal(err)
	}
	var cfg roomctl.Config
	if err := roomctl.LoadJSON(filepath.Join(dir, "config.json"), &cfg); err != nil ||
		cfg != (roomctl.Config{URL: b.srv.URL, Issuer: "https://auth.example", ClientID: "cli", ProjectID: "29184"}) {
		t.Fatalf("%+v %v", cfg, err)
	}
	if err := r.Run(t.Context(), []string{"configure", "--from", "http://rooms.priv.example", "--yes"}); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("a plain-http rooms URL: %v", err)
	}
	if err := r.Run(t.Context(), []string{"configure", "--from", b.srv.URL, "--url", b.srv.URL, "--yes"}); err == nil {
		t.Fatal("--from and --url together are ambiguous")
	}
}

// An http issuer in the broker's answer is refused: the device flow would run in clear.
func TestRoomctlConfigureFromRefusesAnHTTPIssuer(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"issuer":"http://auth.example","clientID":"cli","projectID":"1"}`))
	}))
	t.Cleanup(srv.Close)
	r := Roomctl{Dir: t.TempDir(), HC: srv.Client(), Out: &strings.Builder{}, Err: &strings.Builder{}}
	if err := r.Run(t.Context(), []string{"configure", "--from", srv.URL, "--yes"}); err == nil || !strings.Contains(err.Error(), "issuer") {
		t.Fatalf("%v", err)
	}
}

// Review 1: --from shows what it writes, and what it replaces, before it writes; a terminal is
// asked, --yes skips the question, and an issuer on another registrable domain is refused unless
// --yes says the developer saw it.
func TestRoomctlConfigureFromShowsAndAsks(t *testing.T) {
	b := newRoomctlBroker(t)
	dir := t.TempDir()
	hints := &strings.Builder{}
	r := Roomctl{Dir: dir, HC: b.srv.Client(), Out: &strings.Builder{}, Err: hints}
	stale := roomctl.Config{URL: b.srv.URL, Issuer: "https://old.example", ClientID: "stale", ProjectID: "1"}
	save := func() {
		if err := roomctl.SaveJSON(filepath.Join(dir, "config.json"), stale); err != nil {
			t.Fatal(err)
		}
	}
	load := func() (c roomctl.Config) { _ = roomctl.LoadJSON(filepath.Join(dir, "config.json"), &c); return c }
	save()
	asked := 0
	r.Ask = func(string) (bool, error) { asked++; return false, nil }
	// The broker's issuer (auth.example) is not under the broker's host: it needs --yes whatever the terminal says.
	if err := r.Run(t.Context(), []string{"configure", "--from", b.srv.URL}); err == nil || !strings.Contains(err.Error(), "domain") || asked != 0 || load() != stale {
		t.Fatalf("another domain: %v asked %d %+v", err, asked, load())
	}
	// --yes: no question, and the four values and the old ones are shown.
	hints.Reset()
	if err := r.Run(t.Context(), []string{"configure", "--from", b.srv.URL, "--yes"}); err != nil || asked != 0 {
		t.Fatalf("--yes: %v asked %d", err, asked)
	}
	for _, w := range []string{b.srv.URL, "https://auth.example", "was https://old.example", "cli", "was stale", "29184", "domain"} {
		if !strings.Contains(hints.String(), w) {
			t.Errorf("the hints lack %q:\n%s", w, hints)
		}
	}
	// A terminal is asked when the issuer shares the domain: the answer decides.
	save()
	r.Dir = dir
	same := roomctl.Config{URL: "https://rooms.ogenki.io", Issuer: "https://auth.ogenki.io", ClientID: "cli", ProjectID: "1"}
	if err := r.ConfirmConfig(same, stale, false); err == nil || load() != stale {
		t.Fatalf("a refused question writes nothing: %v", err)
	}
	if asked != 1 {
		t.Fatalf("asked %d", asked)
	}
	r.Ask = func(string) (bool, error) { return true, nil }
	if err := r.ConfirmConfig(same, stale, false); err != nil {
		t.Fatalf("a yes: %v", err)
	}
}

func TestSameRegistrableDomain(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"https://rooms.ogenki.io", "https://auth.ogenki.io", true},
		{"https://rooms.ogenki.io", "https://ogenki.io", true},
		{"https://rooms.ogenki.io", "https://auth.evil.io", false},
		{"https://rooms.a.co.uk", "https://auth.b.co.uk", false},
		{"https://rooms.a.co.uk", "https://auth.a.co.uk", true},
		{"https://127.0.0.1:8443", "https://auth.example", false},
		{"https://127.0.0.1:8443", "https://127.0.0.1:9", true},
		{"https://localhost", "https://auth.localhost", false},
	} {
		if got := sameRegistrableDomain(tc.a, tc.b); got != tc.want {
			t.Errorf("%s vs %s: %v", tc.a, tc.b, got)
		}
	}
}

// O8: setup opencode --restart-service restarts a running background service, and without the
// flag or a terminal it only says so.
func TestRoomctlSetupOpenCodeRestartsTheServiceOnTheFlag(t *testing.T) {
	for flagOn, wantRestart := range map[bool]bool{true: true, false: false} {
		var ran [][]string
		out := &strings.Builder{}
		r := Roomctl{Dir: t.TempDir(), Out: out, Err: &strings.Builder{}, Home: t.TempDir(), Cwd: t.TempDir(),
			LookPath: func(string) (string, error) { return "/usr/bin/opencode", nil },
			RunCmd:   func(_ context.Context, argv []string) error { ran = append(ran, argv); return nil },
			Capture:  func(context.Context, []string) (string, error) { return "http://127.0.0.1:49374\n", nil }}
		args := []string{"setup", "opencode"}
		if flagOn {
			args = append(args, "--restart-service")
		}
		if err := r.Run(t.Context(), args); err != nil {
			t.Fatal(err)
		}
		got := slices.ContainsFunc(ran, func(a []string) bool { return slices.Equal(a, []string{"opencode", "service", "restart"}) })
		if got != wantRestart || !strings.Contains(out.String(), "background service") {
			t.Errorf("flag %v: restarted %v\n%s", flagOn, got, out)
		}
	}
}

// localBroker is a local-mode broker on loopback, over plain HTTP: the listener a port-forward
// reaches. It records every Authorization, so a test proves roomctl sends none.
type localBroker struct {
	srv  *httptest.Server
	mu   sync.Mutex
	auth []string
}

func newLocalBroker(t *testing.T) *localBroker {
	t.Helper()
	b := &localBroker{}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.mu.Lock()
		b.auth = append(b.auth, r.Header.Get("Authorization"))
		b.mu.Unlock()
		switch r.URL.Path {
		case "/.well-known/roomctl":
			_, _ = w.Write([]byte(`{"url":"` + b.srv.URL + `","humanMode":"local"}`))
		case "/api/rooms":
			_, _ = w.Write([]byte(`[{"id":"3kq7x2ma","repository":"Smana/a","phase":"Active","owner":"human:local","lastSeq":42,"you":{"role":"admin"}}]`))
		case "/mcp":
			var m struct {
				ID json.RawMessage `json:"id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&m)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(m.ID) + `,"result":{}}`))
		default:
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
				if wsjson.Write(r.Context(), c, wire.ServerFrame{Type: wire.FrameAck, ClientSeq: f.ClientSeq, Seq: 43}) != nil {
					return
				}
			}
		}
	}))
	t.Cleanup(b.srv.Close)
	return b
}

// configuredLocal is a roomctl directory configured for b, with no token file: a local broker needs
// none.
func configuredLocal(t *testing.T, b *localBroker) (roomctlApp Roomctl, out *strings.Builder) {
	t.Helper()
	out = &strings.Builder{}
	r := Roomctl{Dir: t.TempDir(), HC: b.srv.Client(), Stream: b.srv.Client(), Out: out, Err: &strings.Builder{},
		Home: t.TempDir(), Cwd: t.TempDir()} // a home with no installed skill: doctor's skill check skips
	if err := r.Run(t.Context(), []string{"configure", "--url", b.srv.URL}); err != nil {
		t.Fatal(err)
	}
	return r, out
}

// countingTransport answers every request and counts them: a refusal that must happen before any
// request shows a count of zero.
type countingTransport struct {
	n      int
	answer string
}

func (c *countingTransport) RoundTrip(*http.Request) (*http.Response, error) {
	c.n++
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{},
		Body: io.NopCloser(strings.NewReader(c.answer))}, nil
}

// The URL alone configures a local install: the loopback broker serves humanMode local and no IdP
// values, and roomctl refuses any plain-HTTP host that is not this machine before asking it.
func TestRoomctlConfigureLocal(t *testing.T) {
	b := newLocalBroker(t)
	dir := t.TempDir()
	r := Roomctl{Dir: dir, HC: b.srv.Client(), Stream: b.srv.Client(), Out: &strings.Builder{}, Err: &strings.Builder{}}
	if err := r.Run(t.Context(), []string{"configure", "--url", b.srv.URL + "/"}); err != nil {
		t.Fatal(err)
	}
	var cfg roomctl.Config
	if err := roomctl.LoadJSON(filepath.Join(dir, "config.json"), &cfg); err != nil ||
		cfg != (roomctl.Config{URL: b.srv.URL, HumanMode: roomctl.HumanModeLocal}) {
		t.Fatalf("%+v %v", cfg, err)
	}

	// The same loopback answer, reporting OIDC: the plain-HTTP listener is local mode's alone.
	oidc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"humanMode":"oidc","issuer":"https://auth.example","clientID":"c","projectID":"p"}`))
	}))
	t.Cleanup(oidc.Close)
	if err := r.Run(t.Context(), []string{"configure", "--url", oidc.URL}); err == nil || !strings.Contains(err.Error(), "humanMode") {
		t.Fatalf("a plain-http broker that is not local: %v", err)
	}

	// Not this machine: refused before any request, though the fake would say humanMode local.
	count := &countingTransport{answer: `{"humanMode":"local"}`}
	plain := Roomctl{Dir: t.TempDir(), HC: &http.Client{Transport: count}, Out: &strings.Builder{}, Err: &strings.Builder{}}
	if err := plain.Run(t.Context(), []string{"configure", "--url", "http://rooms.example"}); err == nil {
		t.Fatal("a plain-http broker on another host must be refused")
	}
	if count.n != 0 {
		t.Fatalf("%d requests reached http://rooms.example", count.n)
	}

	// HTTPS discovery is unchanged: the three values come from the broker, and no mode is assumed.
	tls := newRoomctlBroker(t)
	httpsDir := t.TempDir()
	hr := Roomctl{Dir: httpsDir, HC: tls.srv.Client(), Out: &strings.Builder{}, Err: &strings.Builder{}}
	if err := hr.Run(t.Context(), []string{"configure", "--url", tls.srv.URL}); err != nil {
		t.Fatal(err)
	}
	var got roomctl.Config
	if err := roomctl.LoadJSON(filepath.Join(httpsDir, "config.json"), &got); err != nil ||
		got != (roomctl.Config{URL: tls.srv.URL, Issuer: "https://auth.example", ClientID: "cli", ProjectID: "29184"}) {
		t.Fatalf("%+v %v", got, err)
	}
}

// A local broker has no IdP and no token: login and token say so, and no request roomctl makes to it
// carries an Authorization header.
func TestRoomctlLocalCommands(t *testing.T) {
	b := newLocalBroker(t)
	r, out := configuredLocal(t, b)
	if err := r.Run(t.Context(), []string{"login"}); err != nil || !strings.Contains(out.String(), "local broker; no login needed") {
		t.Fatalf("login: %q %v", out, err)
	}
	if _, err := os.Stat(filepath.Join(r.Dir, "token.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("login wrote a token file: %v", err)
	}
	out.Reset()
	if err := r.Run(t.Context(), []string{"token"}); err != nil || out.String() != "local broker: no token required\n" {
		t.Fatalf("token: %q %v", out, err)
	}
	out.Reset()
	if err := r.Run(t.Context(), []string{"rooms", "--format", "text"}); err != nil || !strings.Contains(out.String(), "3kq7x2ma") {
		t.Fatalf("rooms: %q %v", out, err)
	}
	if err := r.Run(t.Context(), []string{"post", "3kq7x2ma", "hello"}); err != nil || !strings.Contains(out.String(), "seq 43") {
		t.Fatalf("post over the WebSocket: %q %v", out, err)
	}
	// MCP: the relay's requests carry no Authorization either.
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	mcp := r
	mcp.In, mcp.Out = inR, outW
	done := make(chan error, 1)
	go func() { done <- mcp.Run(t.Context(), []string{"mcp"}) }()
	if _, err := inW.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(outR).ReadString('\n')
	if err != nil || line != `{"id":1,"jsonrpc":"2.0","result":{}}`+"\n" {
		t.Fatalf("mcp: %q %v", line, err)
	}
	_ = inW.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, a := range b.auth {
		if a != "" {
			t.Fatalf("request %d carried %q: a local broker takes no token", i, a)
		}
	}
	if len(b.auth) < 4 {
		t.Fatalf("only %d requests reached the broker: %v", len(b.auth), b.auth)
	}
}

// Doctor on a local install: no token file and no IdP, the loopback discovery answers, and the room
// list is read without an Authorization header.
func TestDoctorLocal(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		if r.URL.Path == "/api/rooms" {
			auth = r.Header.Get("Authorization")
		}
		mu.Unlock()
		if r.URL.Path == "/api/rooms" {
			_, _ = w.Write([]byte(`[]`))
			return
		}
		_, _ = w.Write([]byte(`{"humanMode":"local"}`))
	}))
	t.Cleanup(srv.Close)
	tokPath := filepath.Join(t.TempDir(), "token.json")
	var out strings.Builder
	d := roomctl.Doctor{Config: roomctl.Config{URL: srv.URL, HumanMode: roomctl.HumanModeLocal}, HC: srv.Client(),
		TokenPath: tokPath, SkillDirs: nil, Version: "dev",
		Lookup: func(context.Context, string) ([]string, error) { return []string{"127.0.0.1"}, nil }}
	if err := d.Run(t.Context(), &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "ok   login: local broker") || !strings.Contains(out.String(), "ok   dns") {
		t.Fatalf("%s", out.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if auth != "" {
		t.Fatalf("the room list carried %q", auth)
	}
	if slices.Contains(paths, "/oauth/v2/token") {
		t.Fatalf("doctor called an IdP: %v", paths)
	}
	if _, err := os.Stat(tokPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("doctor wrote a token file: %v", err)
	}
}
