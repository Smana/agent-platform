// SPDX-License-Identifier: Apache-2.0

package roomctl

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/Smana/agent-platform/internal/roomctl/skill"
)

// #48: each failure the runbook met names its cause and its fix.
func TestCauseNamesWhatWentWrong(t *testing.T) {
	dns := &url.Error{Op: "Get", URL: "https://rooms.priv.example/api/rooms",
		Err: &net.OpError{Op: "dial", Err: &net.DNSError{Err: "no such host", Name: "rooms.priv.example", Server: "127.0.0.53:53", IsNotFound: true}}}
	ca := &url.Error{Op: "Get", URL: "https://rooms.priv.example/api/rooms",
		Err: &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}}
	refused := &url.Error{Op: "Get", URL: "https://rooms.priv.example/api/rooms",
		Err: &net.OpError{Op: "dial", Err: errors.New("connection refused")}}
	for name, tc := range map[string]struct {
		err  error
		want []string
	}{
		"dns": {dns, []string{"rooms.priv.example", "VPN"}},
		"ca":  {ca, []string{"platform CA", "trust"}},
		"timeout": {&url.Error{Op: "Get", URL: "https://rooms.priv.example/", Err: context.DeadlineExceeded},
			[]string{"rooms.priv.example", "VPN"}},
		"refused": {refused, []string{"rooms.priv.example", "refused"}},
	} {
		got := Cause(tc.err)
		for _, w := range tc.want {
			if got == nil || !strings.Contains(got.Error(), w) {
				t.Errorf("%s: %v lacks %q", name, got, w)
			}
		}
		if got != nil && !errors.Is(got, tc.err) {
			t.Errorf("%s: the cause is lost to errors.Is", name)
		}
	}
	other := errors.New("rooms: 403")
	if !errors.Is(Cause(other), other) || Cause(nil) != nil {
		t.Error("an error with no known cause passes through unchanged")
	}
}

func TestCauseOnARealUntrustedCertificate(t *testing.T) {
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	t.Cleanup(srv.Close)
	req, rerr := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	if rerr != nil {
		t.Fatal(rerr)
	}
	resp, err := (&http.Client{}).Do(req)
	if err == nil {
		_ = resp.Body.Close()
		t.Fatal("the default client trusts an httptest certificate")
	}
	if got := Cause(err); got == nil || !strings.Contains(got.Error(), "platform CA") {
		t.Fatalf("%v", got)
	}
}

func TestDiscoverReadsTheBrokersPublicEndpoint(t *testing.T) {
	var path string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		if r.Header.Get("Authorization") != "" {
			t.Error("discovery is unauthenticated")
		}
		_, _ = w.Write([]byte(`{"url":"https://rooms.priv.example","issuer":"https://auth.example","clientID":"3434@agents","projectID":"29184"}`))
	}))
	t.Cleanup(srv.Close)
	got, err := Discover(t.Context(), srv.Client(), srv.URL+"/")
	if err != nil || path != "/.well-known/roomctl" {
		t.Fatalf("%v at %q", err, path)
	}
	// url is the one asked for, not the broker's own idea of it: a proxy may differ.
	if got != (Config{URL: srv.URL, Issuer: "https://auth.example", ClientID: "3434@agents", ProjectID: "29184"}) {
		t.Fatalf("%+v", got)
	}

	empty := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"url":"u","issuer":"https://auth.example","clientID":"","projectID":""}`))
	}))
	t.Cleanup(empty.Close)
	if _, err := Discover(t.Context(), empty.Client(), empty.URL); err == nil || !strings.Contains(err.Error(), "roomctl client") {
		t.Fatalf("a broker with no roomctl client yet: %v", err)
	}
	signin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://auth.example/sign_in", http.StatusFound)
	}))
	t.Cleanup(signin.Close)
	if _, err := Discover(t.Context(), signin.Client(), signin.URL); err == nil || !strings.Contains(err.Error(), "does not serve") {
		t.Fatalf("a proxy that gates the endpoint: %v", err)
	}
}

type doctorRig struct {
	d      Doctor
	access *string
}

func newDoctorRig(t *testing.T) doctorRig {
	t.Helper()
	access := ""
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/roomctl":
			_, _ = w.Write([]byte(`{}`))
		case "/api/rooms":
			if r.Header.Get("Authorization") != "Bearer tok" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if access != "" {
				w.Header().Set("X-Rooms-Access", access)
			}
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	tokPath := filepath.Join(dir, "token.json")
	if err := SaveJSON(tokPath, &oauth2.Token{AccessToken: "tok", TokenType: "Bearer", RefreshToken: "rt", Expiry: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	return doctorRig{access: &access, d: Doctor{Config: Config{URL: srv.URL, Issuer: "https://auth.example", ClientID: "c", ProjectID: "p"},
		HC: srv.Client(), TokenPath: tokPath,
		Lookup: func(context.Context, string) ([]string, error) { return []string{"127.0.0.1"}, nil }}}
}

func TestDoctorPassesWhenEverythingWorks(t *testing.T) {
	r := newDoctorRig(t)
	var out strings.Builder
	if err := r.d.Run(t.Context(), &out); err != nil {
		t.Fatalf("%v\n%s", err, &out)
	}
	for _, w := range []string{"ok   dns", "ok   tls", "ok   login", "ok   github"} {
		if !strings.Contains(out.String(), w) {
			t.Errorf("no %q in:\n%s", w, &out)
		}
	}
}

func TestDoctorNamesTheCauseAndTheFix(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*doctorRig)
		want   []string
	}{
		"vpn down": {func(r *doctorRig) {
			r.d.Lookup = func(context.Context, string) ([]string, error) {
				return nil, &net.DNSError{Err: "no such host", Name: "rooms.priv.example", IsNotFound: true}
			}
		}, []string{"FAIL dns", "connect your VPN"}},
		"ca untrusted":      {func(r *doctorRig) { r.d.HC = &http.Client{} }, []string{"ok   dns", "FAIL tls", "trust the platform CA"}},
		"not logged in":     {func(r *doctorRig) { r.d.TokenPath += ".missing" }, []string{"ok   tls", "FAIL login", "run roomctl login"}},
		"github unlinked":   {func(r *doctorRig) { *r.access = "unlinked" }, []string{"ok   login", "FAIL github", "ask an admin to link"}},
		"github unverified": {func(r *doctorRig) { *r.access = "unverified" }, []string{"FAIL github", "retry"}},
	} {
		t.Run(name, func(t *testing.T) {
			r := newDoctorRig(t)
			tc.mutate(&r)
			var out strings.Builder
			err := r.d.Run(t.Context(), &out)
			if err == nil {
				t.Fatalf("a failing check exits 0:\n%s", &out)
			}
			for _, w := range tc.want {
				if !strings.Contains(out.String(), w) {
					t.Errorf("no %q in:\n%s", w, &out)
				}
			}
		})
	}
}

func TestDoctorSkillDrift(t *testing.T) {
	stamp := func(t *testing.T, dir, v string) {
		t.Helper()
		if _, err := skill.Install(dir, v); err != nil {
			t.Fatal(err)
		}
	}
	for name, tc := range map[string]struct {
		setup   func(t *testing.T, d1, d2 string)
		want    []string
		wantErr bool
	}{
		"drifted": {func(t *testing.T, d1, _ string) { stamp(t, d1, "v0.1.0") },
			[]string{"FAIL skill", "v0.1.0", "v0.2.0", "roomctl setup"}, true},
		"current": {func(t *testing.T, d1, d2 string) { stamp(t, d1, "v0.2.0"); stamp(t, d2, "v0.2.0") },
			[]string{"ok   skill"}, false},
		"one of two drifted": {func(t *testing.T, d1, d2 string) { stamp(t, d1, "v0.2.0"); stamp(t, d2, "v0.1.0") },
			[]string{"FAIL skill", "v0.1.0"}, true},
		"none": {func(*testing.T, string, string) {}, []string{"skip skill"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			r := newDoctorRig(t)
			d1, d2 := t.TempDir(), t.TempDir()
			tc.setup(t, d1, d2)
			r.d.SkillDirs, r.d.Version = []string{d1, d2}, "v0.2.0"
			var out strings.Builder
			err := r.d.Run(t.Context(), &out)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error %v\n%s", err, tc.wantErr, &out)
			}
			for _, w := range tc.want {
				if !strings.Contains(out.String(), w) {
					t.Errorf("no %q in:\n%s", w, &out)
				}
			}
			if tc.wantErr && !strings.Contains(out.String(), d1) && !strings.Contains(out.String(), d2) {
				t.Errorf("the directory is not named:\n%s", &out)
			}
		})
	}
}
