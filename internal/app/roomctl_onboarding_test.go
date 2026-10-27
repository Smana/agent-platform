// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Smana/agent-platform/internal/roomctl"
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
