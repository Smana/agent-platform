// SPDX-License-Identifier: Apache-2.0

package roomctl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeGit puts a git on PATH that runs script, and returns the directory.
func fakeGit(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "git"), []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return dir
}

func TestRunGitTimesOut(t *testing.T) {
	fakeGit(t, "exec sleep 30")
	oldGit, oldFetch := gitTimeout, fetchTimeout
	gitTimeout, fetchTimeout = 100*time.Millisecond, time.Hour
	t.Cleanup(func() { gitTimeout, fetchTimeout = oldGit, oldFetch })
	start := time.Now()
	_, err := RunGit(t.Context(), t.TempDir(), "status")
	if err == nil || !strings.Contains(err.Error(), "timed out") || time.Since(start) > 10*time.Second {
		t.Fatalf("%v after %v", err, time.Since(start))
	}
	// a fetch has its own, longer bound
	gitTimeout, fetchTimeout = time.Hour, 100*time.Millisecond
	start = time.Now()
	if _, err := RunGit(t.Context(), t.TempDir(), "fetch"); err == nil || time.Since(start) > 10*time.Second {
		t.Fatalf("fetch: %v after %v", err, time.Since(start))
	}
}

func TestRunGitCapsOutput(t *testing.T) {
	// 2 MiB on stdout is too much for status and fine for diff and ls-tree; stderr is capped at 64 KiB.
	fakeGit(t, `head -c 2097152 /dev/zero | tr '\0' 'a'; head -c 300000 /dev/zero | tr '\0' 'e' >&2; [ "$5" = fail ] && exit 1; exit 0`)
	if _, err := RunGit(t.Context(), t.TempDir(), "status"); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("status: %v", err)
	}
	for _, c := range []string{"diff", "ls-tree"} {
		out, err := RunGit(t.Context(), t.TempDir(), c)
		if err != nil || len(out) != 2<<20 {
			t.Fatalf("%s: %d bytes, %v", c, len(out), err)
		}
	}
	// past 16 MiB, diff fails closed with a message that says what to do
	fakeGit(t, `head -c 17825792 /dev/zero | tr '\0' 'a'`)
	if _, err := RunGit(t.Context(), t.TempDir(), "diff"); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("diff: %v", err)
	}
	fakeGit(t, `head -c 300000 /dev/zero | tr '\0' 'e' >&2; exit 1`)
	_, err := RunGit(t.Context(), t.TempDir(), "fetch", "x")
	if err == nil || len(err.Error()) > 70000 {
		t.Fatalf("stderr is capped: %d", len(err.Error()))
	}
}

func TestRunGitNeutralisesSubmodulesAndFilters(t *testing.T) {
	dir := fakeGit(t, `printf '%s\n' "$@" > "`+"$(dirname $0)"+`/args"`)
	if _, err := RunGit(t.Context(), t.TempDir(), "status"); err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	for _, want := range []string{"core.hooksPath=/dev/null", "submodule.recurse=false", "fetch.recurseSubmodules=false", "filter.lfs.smudge="} {
		if !strings.Contains(string(args), want+"\n") {
			t.Errorf("%s is not set: %q", want, args)
		}
	}
}

// ls-remote and worktree add may take as long as a fetch.
func TestRunGitBoundsTheSlowCallsLikeAFetch(t *testing.T) {
	fakeGit(t, "sleep 1")
	oldGit, oldFetch := gitTimeout, fetchTimeout
	gitTimeout, fetchTimeout = 200*time.Millisecond, time.Hour
	t.Cleanup(func() { gitTimeout, fetchTimeout = oldGit, oldFetch })
	for _, args := range [][]string{{"ls-remote", "origin"}, {"worktree", "add", "p", "x"}} {
		if _, err := RunGit(t.Context(), t.TempDir(), args...); err != nil {
			t.Errorf("%v: %v", args, err)
		}
	}
	if _, err := RunGit(t.Context(), t.TempDir(), "worktree", "list"); err == nil {
		t.Error("worktree list keeps the short bound")
	}
}
