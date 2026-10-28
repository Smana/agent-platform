// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Smana/maku/internal/maku"
)

func TestMakuRunFlags(t *testing.T) {
	r := Maku{Dir: t.TempDir(), Out: &strings.Builder{}, Err: &strings.Builder{}}
	if err := maku.SaveJSON(filepath.Join(r.Dir, "config.json"), maku.Config{URL: "http://localhost:8080", HumanMode: maku.HumanModeLocal}); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"run"},
		{"run", "--task", "x"},
		{"run", "--repo", "owner/repo"},
		{"run", "--repo", "not a repo", "--task", "x"},
		{"run", "--repo", "owner/repo", "--task", "   "},
		{"run", "--repo", "owner/repo", "--task", "x", "extra"},
	} {
		if err := r.Run(t.Context(), args); err == nil || !strings.Contains(err.Error(), "--repo owner/name --task TEXT") {
			t.Errorf("%q: %v", args, err)
		}
	}
}

// run is quickstart-only: against a production (OIDC) broker it refuses before any request.
func TestMakuRunIsQuickstartOnly(t *testing.T) {
	b := newMakuBroker(t)
	r, _ := configured(t, b)
	b.mu.Lock()
	b.query = ""
	b.reqs = 0 // configure asked the setup values
	b.mu.Unlock()
	err := r.Run(t.Context(), []string{"run", "--repo", "owner/repo", "--task", "x"})
	if err == nil || !strings.Contains(err.Error(), "quickstart-only") {
		t.Fatalf("err %v", err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.acts) != 0 || b.query != "" || b.reqs != 0 {
		t.Fatalf("acts %v, request %q, %d requests", b.acts, b.query, b.reqs)
	}
}

// A config saved as local whose https URL is an OIDC broker is refused on what the broker reports,
// before any /api request: only a broker that says it is local is acted on.
func TestMakuRunRefusesABrokerThatIsNotLocal(t *testing.T) {
	var posts atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/roomctl" {
			posts.Add(1)
		}
		_, _ = w.Write([]byte(`{"url":"https://elsewhere","issuer":"https://auth.example","clientID":"cli","projectID":"29184"}`))
	}))
	defer srv.Close()
	r := Maku{Dir: t.TempDir(), HC: srv.Client(), Stream: srv.Client(), Out: &strings.Builder{}, Err: &strings.Builder{},
		Apply: func(context.Context, []byte) error { t.Fatal("applied"); return nil }}
	if err := maku.SaveJSON(filepath.Join(r.Dir, "config.json"), maku.Config{URL: srv.URL, HumanMode: maku.HumanModeLocal}); err != nil {
		t.Fatal(err)
	}
	err := r.Run(t.Context(), []string{"run", "--repo", "owner/repo", "--task", "x"})
	if err == nil || !strings.Contains(err.Error(), "quickstart-only") || posts.Load() != 0 {
		t.Fatalf("err %v, requests past discovery %d", err, posts.Load())
	}
}

// A port-forward on another port than publicURL's is refused at the preflight discovery, before
// a room exists.
func TestMakuRunChecksTheForwardedPort(t *testing.T) {
	var posts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts.Add(1)
		}
		_, _ = w.Write([]byte(`{"url":"http://localhost:1","humanMode":"local"}`))
	}))
	defer srv.Close()
	r := Maku{Dir: t.TempDir(), HC: srv.Client(), Stream: srv.Client(), Out: &strings.Builder{}, Err: &strings.Builder{},
		Apply: func(context.Context, []byte) error { t.Fatal("applied"); return nil }}
	if err := maku.SaveJSON(filepath.Join(r.Dir, "config.json"), maku.Config{URL: srv.URL, HumanMode: maku.HumanModeLocal}); err != nil {
		t.Fatal(err)
	}
	err := r.Run(t.Context(), []string{"run", "--repo", "owner/repo", "--task", "x"})
	if err == nil || !strings.Contains(err.Error(), "answers only on port 1") || posts.Load() != 0 {
		t.Fatalf("err %v, posts %d", err, posts.Load())
	}
}

// kubectlApply runs the developer's kubectl with the manifest on stdin (never argv), and kubectl's
// own words reach the developer unchanged. The kubectl here is a fake script on PATH.
func TestKubectlApply(t *testing.T) {
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > \"$FAKE_KUBECTL_DIR/argv\"\ncat > \"$FAKE_KUBECTL_DIR/stdin\"\n" +
		"if [ -n \"$FAKE_KUBECTL_FAIL\" ]; then echo \"$FAKE_KUBECTL_FAIL\" >&2; exit 1; fi\necho 'agentrun.agents.ogenki.io/run-a2b3c4d5 created'\n"
	if err := os.WriteFile(filepath.Join(dir, "kubectl"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("FAKE_KUBECTL_DIR", dir)
	manifest := []byte(`{"kind":"AgentRun"}`)
	var out strings.Builder
	if err := kubectlApply(&out)(t.Context(), manifest); err != nil {
		t.Fatal(err)
	}
	argv, _ := os.ReadFile(filepath.Join(dir, "argv"))
	stdin, _ := os.ReadFile(filepath.Join(dir, "stdin"))
	if string(argv) != "apply -f -\n" || string(stdin) != string(manifest) || !strings.Contains(out.String(), "run-a2b3c4d5 created") {
		t.Fatalf("argv %q stdin %q out %q", argv, stdin, out.String())
	}
	t.Setenv("FAKE_KUBECTL_FAIL", `error: the server doesn't have a resource type "agentruns"`)
	out.Reset()
	err := kubectlApply(&out)(t.Context(), manifest)
	if err == nil || !strings.Contains(out.String(), `resource type "agentruns"`) {
		t.Fatalf("err %v out %q", err, out.String())
	}
}
