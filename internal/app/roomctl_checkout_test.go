// SPDX-License-Identifier: Apache-2.0

package app

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// checkoutWorkspace is a clone named a of a bare origin that has agent/3kq7x2ma, which adds files.
// The clone records a GitHub URL that insteadOf sends to the bare repository.
func checkoutWorkspace(t *testing.T, files map[string]string) (ws, parent string) {
	t.Helper()
	root, _ := filepath.EvalSymlinks(t.TempDir())
	git := func(dir string, args ...string) {
		t.Helper()
		argv := append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false",
			"-c", "protocol.file.allow=always", "-c", "core.excludesFile=/dev/null", "-C", dir}, args...)
		cmd := exec.CommandContext(t.Context(), "git", argv...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	origin, seed := filepath.Join(root, "origin.git"), filepath.Join(root, "seed")
	parent = filepath.Join(root, "work")
	for _, d := range []string{origin, seed, parent} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	git(origin, "init", "-q", "--bare", "-b", "main")
	git(seed, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(seed, "add", "-A")
	git(seed, "commit", "-q", "-m", "init")
	git(seed, "remote", "add", "origin", origin)
	git(seed, "push", "-q", "origin", "main")
	git(seed, "checkout", "-q", "-b", "agent/3kq7x2ma")
	for rel, body := range files {
		if err := os.WriteFile(filepath.Join(seed, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git(seed, "add", "-A")
	git(seed, "commit", "-q", "-m", "work")
	git(seed, "push", "-q", "origin", "agent/3kq7x2ma")
	ws = filepath.Join(parent, "a")
	git(parent, "clone", "-q", origin, ws)
	git(ws, "remote", "set-url", "origin", "git@github.com:Smana/a.git")
	git(ws, "config", "url."+origin+".insteadOf", "git@github.com:Smana/a.git")
	return ws, parent
}

func TestRoomctlCheckoutNeedsATTY(t *testing.T) {
	ws, parent := checkoutWorkspace(t, map[string]string{".mcp.json": "{}\n"})
	r, out := configured(t, newRoomctlBroker(t))
	r.Cwd, r.In = ws, strings.NewReader("y\n") // an answer that is not from a terminal does not count
	err := r.Run(t.Context(), []string{"checkout", "3kq7x2ma"})
	if err == nil || !strings.Contains(err.Error(), ".mcp.json") {
		t.Fatalf("%v", err)
	}
	if _, err := os.Stat(filepath.Join(parent, "a.3kq7x2ma")); err == nil || out.Len() != 0 {
		t.Fatalf("created, or printed %q", out)
	}
}

func TestRoomctlCheckoutCreatesAWorktreeWithoutFlaggedFiles(t *testing.T) {
	ws, parent := checkoutWorkspace(t, map[string]string{"main.go": "package main\n"})
	r, out := configured(t, newRoomctlBroker(t))
	r.Cwd = ws
	if err := r.Run(t.Context(), []string{"checkout", "3kq7x2ma"}); err != nil {
		t.Fatal(err)
	}
	if got, want := strings.TrimSpace(out.String()), filepath.Join(parent, "a.3kq7x2ma"); got != want {
		t.Fatalf("%q, want %q", got, want)
	}
	if err := r.Run(t.Context(), []string{"checkout", "aaaaaaaa"}); err == nil {
		t.Fatal("an unknown room")
	}
	if err := r.Run(t.Context(), []string{"checkout"}); err == nil {
		t.Fatal("no room")
	}
}
