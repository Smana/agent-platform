// SPDX-License-Identifier: Apache-2.0

package roomctl

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
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
	rooms  *string           // GET /api/rooms' body
	draft  *string           // issue_draft's structuredContent
	byRepo map[string]string // issue_draft's structuredContent for one repository, over draft
}

func newDoctorRig(t *testing.T) doctorRig {
	t.Helper()
	access, rooms, draft := "", `[]`, `{"canStart":false,"canStartWhy":"off"}`
	byRepo := map[string]string{}
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
			_, _ = w.Write([]byte(rooms))
		case "/mcp":
			var m struct {
				Params struct {
					Arguments struct{ Repo string } `json:"arguments"`
				} `json:"params"`
			}
			_ = json.NewDecoder(r.Body).Decode(&m)
			sc := draft
			if v, ok := byRepo[m.Params.Arguments.Repo]; ok {
				sc = v
			}
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"x","result":{"content":[{"type":"text","text":"d"}],"structuredContent":` + sc + `}}`))
		}
	}))
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	tokPath := filepath.Join(dir, "token.json")
	if err := SaveJSON(tokPath, &oauth2.Token{AccessToken: "tok", TokenType: "Bearer", RefreshToken: "rt", Expiry: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	return doctorRig{access: &access, rooms: &rooms, draft: &draft, byRepo: byRepo, d: Doctor{Config: Config{URL: srv.URL, Issuer: "https://auth.example", ClientID: "c", ProjectID: "p"},
		HC: srv.Client(), TokenPath: tokPath,
		Lookup: func(context.Context, string) ([]string, error) { return []string{"127.0.0.1"}, nil }}}
}

func TestDoctorPassesWhenEverythingWorks(t *testing.T) {
	r := newDoctorRig(t)
	*r.rooms, *r.draft = `[{"repository":"Smana/a"}]`, `{"canStart":true}`
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
		"unreadable": {func(t *testing.T, d1, _ string) {
			stamp(t, d1, "v0.2.0")
			p := filepath.Join(d1, "factory-handoff", "SKILL.md")
			if err := os.Chmod(p, 0); err != nil {
				t.Fatal(err)
			}
			if _, err := os.ReadFile(p); err == nil {
				t.Skip("running as a user that reads mode 000 files")
			}
		}, []string{"FAIL skill"}, true},
		"symlinked skill dir": {func(t *testing.T, d1, d2 string) {
			stamp(t, d2, "v0.1.0")
			if err := os.Remove(d1); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(d2, d1); err != nil {
				t.Fatal(err)
			}
		}, []string{"FAIL skill", "v0.1.0"}, true},
		"nonexistent dir": {func(t *testing.T, d1, d2 string) {
			if err := os.Remove(d1); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(d2); err != nil {
				t.Fatal(err)
			}
		}, []string{"skip skill"}, false},
		"no stamp": {func(t *testing.T, d1, _ string) {
			stamp(t, d1, "v0.2.0")
			p := filepath.Join(d1, "factory-handoff", "SKILL.md")
			if err := os.WriteFile(p, []byte("---\nname: factory-handoff\n---\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, []string{"FAIL skill", "roomctl-version"}, true},
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
			if tc.wantErr {
				// The directory that drifted is the one named on the FAIL line, not the current one.
				bad := d1
				if name == "one of two drifted" {
					bad = d2
				}
				named := false
				for _, l := range strings.Split(out.String(), "\n") {
					if strings.HasPrefix(l, "FAIL skill") && strings.Contains(l, bad) {
						named = true
					}
				}
				if !named {
					t.Errorf("no FAIL skill line names %s:\n%s", bad, &out)
				}
			}
		})
	}
}

func TestDoctorSkillCheckRunsWithAnUnusableURL(t *testing.T) {
	dir := t.TempDir()
	if _, err := skill.Install(dir, "v0.1.0"); err != nil {
		t.Fatal(err)
	}
	d := Doctor{Config: Config{URL: "::not a url"}, SkillDirs: []string{dir}, Version: "v0.2.0"}
	var out strings.Builder
	if err := d.Run(t.Context(), &out); err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(out.String(), "FAIL skill") {
		t.Errorf("the skill check did not run:\n%s", &out)
	}
}

// O3b: the room list answers "ok" for an admin whatever the GitHub link, so the header cannot say
// "linked". doctor asks issue_draft, as the broker's canStart does, and tells what it learns.
func TestDoctorGitHubLinkIsNeverAssumed(t *testing.T) {
	const room = `[{"id":"3kq7x2ma","repository":"Smana/a"}]`
	for name, tc := range map[string]struct {
		access, rooms, draft string
		want, wantNot        []string
		fails                bool
	}{
		"admin, not linked": {"ok", room, `{"canStart":false,"canStartWhy":"not_linked"}`,
			[]string{"FAIL github", "no linked GitHub identity"}, []string{"identity is linked"}, true},
		"linked":               {"ok", room, `{"canStart":true}`, []string{"ok   github", "identity is linked"}, nil, false},
		"cannot say":           {"ok", room, `{"canStart":false,"canStartWhy":"off"}`, []string{"warn github", "unverified"}, []string{"identity is linked"}, false},
		"no room to ask about": {"", `[]`, `{"canStart":true}`, []string{"warn github", "unverified"}, []string{"identity is linked"}, false},
	} {
		t.Run(name, func(t *testing.T) {
			r := newDoctorRig(t)
			*r.access, *r.rooms, *r.draft = tc.access, tc.rooms, tc.draft
			var out strings.Builder
			err := r.d.Run(t.Context(), &out)
			if (err != nil) != tc.fails {
				t.Fatalf("err %v\n%s", err, &out)
			}
			for _, w := range tc.want {
				if !strings.Contains(out.String(), w) {
					t.Errorf("no %q in:\n%s", w, &out)
				}
			}
			for _, w := range tc.wantNot {
				if strings.Contains(out.String(), w) {
					t.Errorf("%q in:\n%s", w, &out)
				}
			}
		})
	}
}

// Review 6: the probe tries another repository when the first one cannot answer (not in
// sessionStart), up to three, and "unverified" is a warning that does not fail the run.
func TestDoctorProbeTriesOtherRepositories(t *testing.T) {
	rooms := `[{"repository":"Smana/a"},{"repository":"Smana/a"},{"repository":"Smana/b"},{"repository":"Smana/c"}]`
	for name, tc := range map[string]struct {
		byRepo map[string]string
		want   string
		fails  bool
	}{
		"second repository says not linked":  {map[string]string{"Smana/b": `{"canStart":false,"canStartWhy":"not_linked"}`}, "FAIL github", true},
		"second repository says linked":      {map[string]string{"Smana/b": `{"canStart":true}`}, "ok   github: your GitHub identity is linked", false},
		"none can say":                       {nil, "warn github", false},
		"the fourth repository is not asked": {map[string]string{"Smana/d": `{"canStart":true}`}, "warn github", false},
	} {
		t.Run(name, func(t *testing.T) {
			r := newDoctorRig(t)
			*r.rooms = rooms
			if name == "the fourth repository is not asked" {
				*r.rooms = `[{"repository":"Smana/a"},{"repository":"Smana/b"},{"repository":"Smana/c"},{"repository":"Smana/d"}]`
			}
			for k, v := range tc.byRepo {
				r.byRepo[k] = v
			}
			var out strings.Builder
			if err := r.d.Run(t.Context(), &out); (err != nil) != tc.fails || !strings.Contains(out.String(), tc.want) {
				t.Fatalf("err %v\n%s", err, &out)
			}
		})
	}
}

// O8: the doctor warns when OpenCode's background service started before its config changed.
func TestDoctorOpenCodeServiceStartedBeforeTheConfig(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "opencode.json")
	if err := os.WriteFile(cfg, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	changed := time.Now().Add(-time.Hour)
	if err := os.Chtimes(cfg, changed, changed); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		start   func() (time.Time, bool)
		want    []string
		wantNot []string
	}{
		"started before the config": {func() (time.Time, bool) { return changed.Add(-time.Hour), true },
			[]string{"warn opencode", "opencode service restart"}, []string{"FAIL"}},
		"started after the config": {func() (time.Time, bool) { return changed.Add(time.Minute), true },
			[]string{"ok   opencode"}, []string{"warn opencode"}},
		"no service running": {func() (time.Time, bool) { return time.Time{}, false }, nil, []string{"opencode:"}},
		"not asked":          {nil, nil, []string{"opencode:"}},
	} {
		t.Run(name, func(t *testing.T) {
			r := newDoctorRig(t)
			r.d.OpenCodeConfigs, r.d.ServiceStart = []string{cfg, cfg + ".missing"}, tc.start
			var out strings.Builder
			if err := r.d.Run(t.Context(), &out); err != nil {
				t.Fatalf("%v\n%s", err, &out)
			}
			for _, w := range tc.want {
				if !strings.Contains(out.String(), w) {
					t.Errorf("no %q in:\n%s", w, &out)
				}
			}
			for _, w := range tc.wantNot {
				if strings.Contains(out.String(), w) {
					t.Errorf("%q in:\n%s", w, &out)
				}
			}
		})
	}
}

// O9: the doctor warns when an `execute` deny rule hides Code Mode servers and the factory entry
// does not opt out with codemode:false.
func TestDoctorOpenCodeCodeMode(t *testing.T) {
	deny := `"permission":[{"action":"execute","resource":"*","effect":"deny"}]`
	for name, tc := range map[string]struct {
		config string
		warn   bool
	}{
		"deny, no opt-out":   {`{` + deny + `,"mcp":{"servers":{"factory":{"type":"local"}}}}`, true},
		"deny, opted out":    {`{` + deny + `,"mcp":{"servers":{"factory":{"codemode":false}}}}`, false},
		"no deny":            {`{"mcp":{"servers":{"factory":{"type":"local"}}}}`, false},
		"deny, no factory":   {`{` + deny + `,"mcp":{"servers":{}}}`, false},
		"another effect":     {`{"permission":[{"action":"execute","resource":"*","effect":"allow"}],"mcp":{"servers":{"factory":{}}}}`, false},
		"deny nested deeper": {`{"agent":{"build":{"rules":[{"action":"execute","resource":"*","effect":"deny"}]}},"mcp":{"servers":{"factory":{}}}}`, true},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := filepath.Join(t.TempDir(), "opencode.json")
			// the execution timeout is O10's concern: these cases have it
			conf := regexp.MustCompile(`"factory":\{(\}?)`).ReplaceAllStringFunc(tc.config, func(m string) string {
				if strings.HasSuffix(m, "}") {
					return `"factory":{"timeout":{"execution":900000}}`
				}
				return `"factory":{"timeout":{"execution":900000},`
			})
			if err := os.WriteFile(cfg, []byte(conf), 0o644); err != nil {
				t.Fatal(err)
			}
			r := newDoctorRig(t)
			r.d.OpenCodeConfigs = []string{cfg}
			var out strings.Builder
			if err := r.d.Run(t.Context(), &out); err != nil {
				t.Fatalf("%v\n%s", err, &out)
			}
			if got := strings.Contains(out.String(), "warn opencode"); got != tc.warn || (tc.warn && !strings.Contains(out.String(), "codemode")) {
				t.Fatalf("warn=%v want %v\n%s", got, tc.warn, &out)
			}
		})
	}
}

// countingRoundTrip answers nothing and counts: a refusal that must happen before any request shows
// a count of zero.
type countingRoundTrip struct {
	n      int
	answer string
}

func (c *countingRoundTrip) RoundTrip(*http.Request) (*http.Response, error) {
	c.n++
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{},
		Body: io.NopCloser(strings.NewReader(c.answer))}, nil
}

// A local broker is discovered over plain HTTP on this machine only, and its answer says a local
// install needs no IdP values. Every other plain-HTTP host is refused before the request.
func TestDiscoverLocal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/roomctl" || r.Header.Get("Authorization") != "" {
			t.Errorf("%s with %q: discovery is unauthenticated", r.URL.Path, r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"url":"http://` + r.Host + `","humanMode":"local"}`))
	}))
	t.Cleanup(srv.Close)
	got, err := Discover(t.Context(), srv.Client(), srv.URL+"/")
	if err != nil || got != (Config{URL: srv.URL, HumanMode: HumanModeLocal}) {
		t.Fatalf("%+v %v", got, err)
	}

	// Another host in clear is refused before any request, though the answer would say local.
	count := &countingRoundTrip{answer: `{"humanMode":"local"}`}
	if _, err := Discover(t.Context(), &http.Client{Transport: count}, "http://rooms.example"); err == nil ||
		!strings.Contains(err.Error(), "loopback") {
		t.Fatalf("a plain-http broker on another host: %v", err)
	}
	if count.n != 0 {
		t.Fatalf("%d requests reached http://rooms.example", count.n)
	}

	// A loopback answer that reports OIDC is a broker roomctl must not read without TLS.
	oidc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"humanMode":"oidc","issuer":"https://auth.example","clientID":"c","projectID":"p"}`))
	}))
	t.Cleanup(oidc.Close)
	if _, err := Discover(t.Context(), oidc.Client(), oidc.URL); err == nil || !strings.Contains(err.Error(), "humanMode") {
		t.Fatalf("a plain-http broker that is not local: %v", err)
	}

	// An OIDC broker with no roomctl client yet still names what is missing.
	missing := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"humanMode":"oidc","issuer":"https://auth.example","clientID":"","projectID":""}`))
	}))
	t.Cleanup(missing.Close)
	if _, err := Discover(t.Context(), missing.Client(), missing.URL); err == nil || !strings.Contains(err.Error(), "roomctl client") {
		t.Fatalf("an oidc broker with no roomctl client: %v", err)
	}
}

// Doctor on a local install reads no token file and calls no IdP: the loopback listener answers, and
// the room list is read without an Authorization header.
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
	d := Doctor{Config: Config{URL: srv.URL, HumanMode: HumanModeLocal}, HC: srv.Client(), TokenPath: tokPath, Version: "dev",
		Lookup: func(context.Context, string) ([]string, error) { return []string{"127.0.0.1"}, nil }}
	if err := d.Run(t.Context(), &out); err != nil {
		t.Fatalf("%v\n%s", err, &out)
	}
	if !strings.Contains(out.String(), "ok   login: local broker") || !strings.Contains(out.String(), "skip skill") {
		t.Fatalf("%s", out.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if auth != "" {
		t.Fatalf("the room list carried %q", auth)
	}
	for _, p := range paths {
		if p != "/.well-known/roomctl" && p != "/api/rooms" {
			t.Fatalf("doctor called %s", p)
		}
	}
	if _, err := os.Stat(tokPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("doctor wrote a token file: %v", err)
	}
}

// A local broker's Host check answers publicURL's port only: discovery refuses another port up
// front, with the port-forward that fixes it, instead of letting every later call get 403.
func TestDiscoverLocalPortAgreement(t *testing.T) {
	var answer atomic.Value // the test writes it, the server goroutine reads it
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(answer.Load().(string))) }))
	defer srv.Close()
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	for _, tc := range []struct {
		url, wantErr string
	}{
		{fmt.Sprintf("http://localhost:%d", port), ""},
		{fmt.Sprintf("http://[::1]:%d", port), ""},
		{"", ""}, // an older broker sends no url: not checked
		{"http://localhost:1", "answers only on port 1"},
		{"http://localhost", "names no port"},
	} {
		answer.Store(`{"url":"` + tc.url + `","humanMode":"local"}`)
		_, err := Discover(t.Context(), srv.Client(), srv.URL)
		if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
			t.Errorf("url %q: %v, want %q", tc.url, err, tc.wantErr)
		}
	}
}

// O10: OpenCode cuts a tool at its execution timeout, which cut issue_file's form. The doctor warns
// when the factory entry has none.
func TestDoctorOpenCodeExecutionTimeout(t *testing.T) {
	for name, tc := range map[string]struct {
		config string
		warn   bool
	}{
		"none":           {`{"mcp":{"servers":{"factory":{"type":"local","codemode":false}}}}`, true},
		"catalog only":   {`{"mcp":{"servers":{"factory":{"timeout":{"catalog":5000}}}}}`, true},
		"set":            {`{"mcp":{"servers":{"factory":{"timeout":{"execution":900000}}}}}`, false},
		"another server": {`{"mcp":{"servers":{"other":{}}}}`, false},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := filepath.Join(t.TempDir(), "opencode.json")
			if err := os.WriteFile(cfg, []byte(tc.config), 0o644); err != nil {
				t.Fatal(err)
			}
			r := newDoctorRig(t)
			r.d.OpenCodeConfigs = []string{cfg}
			var out strings.Builder
			if err := r.d.Run(t.Context(), &out); err != nil {
				t.Fatalf("%v\n%s", err, &out)
			}
			if got := strings.Contains(out.String(), "warn opencode"); got != tc.warn || (tc.warn && !strings.Contains(out.String(), "execution")) {
				t.Fatalf("warn=%v want %v\n%s", got, tc.warn, &out)
			}
		})
	}
}
