// SPDX-License-Identifier: Apache-2.0

package app

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
	if !strings.Contains(out.String(), "ok   github") {
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
	if err := r.Run(t.Context(), []string{"configure", "--from", b.srv.URL + "/r/26zfnuxm"}); err != nil {
		t.Fatal(err)
	}
	var cfg roomctl.Config
	if err := roomctl.LoadJSON(filepath.Join(dir, "config.json"), &cfg); err != nil ||
		cfg != (roomctl.Config{URL: b.srv.URL, Issuer: "https://auth.example", ClientID: "cli", ProjectID: "29184"}) {
		t.Fatalf("%+v %v", cfg, err)
	}
	if err := r.Run(t.Context(), []string{"configure", "--from", "http://rooms.priv.example"}); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("a plain-http rooms URL: %v", err)
	}
	if err := r.Run(t.Context(), []string{"configure", "--from", b.srv.URL, "--url", b.srv.URL}); err == nil {
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
	if err := r.Run(t.Context(), []string{"configure", "--from", srv.URL}); err == nil || !strings.Contains(err.Error(), "issuer") {
		t.Fatalf("%v", err)
	}
}
