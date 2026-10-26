// SPDX-License-Identifier: Apache-2.0

package roomctl

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// gitIn runs real git in dir, as a test author with no hook and no global config.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	argv := append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false",
		"-c", "protocol.file.allow=always", "-c", "core.excludesFile=/dev/null", "-C", dir}, args...)
	cmd := exec.CommandContext(t.Context(), "git", argv...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func put(t *testing.T, root, rel, body string, mode os.FileMode) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
}

type fixture struct {
	origin, ws, parent string
	marker             string // the file the clone's hooks touch
}

type fixtureOpts struct {
	branch   map[string]string // files the agent branch adds, nil: no agent branch
	url      string            // the origin URL the clone records; default git@github.com:Smana/a.git
	hooks    bool              // main carries .husky hooks, and the clone sets core.hooksPath=.husky
	hooksDir string            // an in-tree core.hooksPath other than .husky
}

// newFixture is a bare origin with main and, maybe, agent/26zfnuxm, and a clone named a as the
// workspace. The clone records a GitHub URL, which insteadOf sends to the bare repository.
func newFixture(t *testing.T, o fixtureOpts) fixture {
	t.Helper()
	root := t.TempDir()
	root, _ = filepath.EvalSymlinks(root)
	f := fixture{origin: filepath.Join(root, "origin.git"), parent: filepath.Join(root, "work"), marker: filepath.Join(root, "marker")}
	seed := filepath.Join(root, "seed")
	for _, d := range []string{f.origin, seed, f.parent} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	gitIn(t, f.origin, "init", "-q", "--bare", "-b", "main")
	gitIn(t, seed, "init", "-q", "-b", "main")
	put(t, seed, "README.md", "hello\n", 0o644)
	if o.hooks {
		for _, h := range []string{"post-checkout", "reference-transaction"} {
			put(t, seed, ".husky/"+h, "#!/bin/sh\ntouch '"+f.marker+"."+h+"'\n", 0o755)
		}
	}
	gitIn(t, seed, "add", "-A")
	gitIn(t, seed, "commit", "-q", "-m", "init")
	gitIn(t, seed, "remote", "add", "origin", f.origin)
	gitIn(t, seed, "push", "-q", "origin", "main")
	if o.branch != nil {
		gitIn(t, seed, "checkout", "-q", "-b", "agent/26zfnuxm")
		for rel, body := range o.branch {
			put(t, seed, rel, body, 0o755)
		}
		gitIn(t, seed, "add", "-A")
		gitIn(t, seed, "commit", "-q", "-m", "agent work")
		gitIn(t, seed, "push", "-q", "origin", "agent/26zfnuxm")
	}
	f.ws = filepath.Join(f.parent, "a")
	gitIn(t, f.parent, "clone", "-q", f.origin, f.ws)
	url := o.url
	if url == "" {
		url = "git@github.com:Smana/a.git"
	}
	gitIn(t, f.ws, "remote", "set-url", "origin", url)
	gitIn(t, f.ws, "config", "url."+f.origin+".insteadOf", url)
	if o.hooks {
		gitIn(t, f.ws, "config", "core.hooksPath", ".husky")
	}
	if o.hooksDir != "" {
		gitIn(t, f.ws, "config", "core.hooksPath", o.hooksDir)
	}
	return f
}

func (f fixture) checkout(g Git) Checkout {
	return Checkout{Git: g, Workspace: f.ws, Room: "26zfnuxm", Repository: "Smana/a", PR: 7}
}

// confirmer records what it was asked and answers yes or no.
type confirmer struct {
	asked  [][]string
	answer bool
}

func (c *confirmer) confirm(_ context.Context, files []string) (bool, error) {
	c.asked = append(c.asked, slices.Clone(files))
	return c.answer, nil
}

func asRefusal(t *testing.T, err error) *CheckoutRefusal {
	t.Helper()
	var r *CheckoutRefusal
	if !errors.As(err, &r) {
		t.Fatalf("not a refusal: %v", err)
	}
	return r
}

func TestCheckoutCreatesAWorktree(t *testing.T) {
	f := newFixture(t, fixtureOpts{branch: map[string]string{"src/x.go": "package x\n"}})
	put(t, f.ws, "dirty.txt", "untracked\n", 0o644)
	put(t, f.ws, "README.md", "edited\n", 0o644)
	head := gitIn(t, f.ws, "rev-parse", "HEAD")
	status := gitIn(t, f.ws, "status", "--porcelain")
	got, err := f.checkout(RunGit).Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(f.parent, "a.26zfnuxm"); got.Path != want || got.Branch != "agent/26zfnuxm" || got.Reused ||
		got.PR != "https://github.com/Smana/a/pull/7" {
		t.Fatalf("%+v", got)
	}
	if b := gitIn(t, got.Path, "branch", "--show-current"); b != "agent/26zfnuxm" {
		t.Fatalf("worktree on %q", b)
	}
	if _, err := os.Stat(filepath.Join(got.Path, "src", "x.go")); err != nil {
		t.Fatal(err)
	}
	if gitIn(t, f.ws, "rev-parse", "HEAD") != head || gitIn(t, f.ws, "branch", "--show-current") != "main" ||
		gitIn(t, f.ws, "status", "--porcelain") != status {
		t.Fatal("the current checkout changed")
	}
}

func TestCheckoutReusesItsWorktree(t *testing.T) {
	f := newFixture(t, fixtureOpts{branch: map[string]string{"src/x.go": "package x\n"}})
	first, err := f.checkout(RunGit).Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	rec := func(ctx context.Context, dir string, args ...string) (string, error) {
		calls = append(calls, args[0])
		return RunGit(ctx, dir, args...)
	}
	second, err := f.checkout(rec).Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if second.Path != first.Path || !second.Reused {
		t.Fatalf("%+v", second)
	}
	if slices.Contains(calls, "fetch") || slices.Contains(calls, "worktree") && slices.Contains(calls, "add") {
		t.Fatalf("a reuse fetched or added: %v", calls)
	}
}

// fixtureFile is a file that matches a flagged entry.
func fixtureFile(entry string) string {
	if strings.HasSuffix(entry, "/") {
		return entry + "x"
	}
	return entry
}

func TestCheckoutConfirmsEachFlaggedPath(t *testing.T) {
	for _, entry := range flagged {
		file := fixtureFile(entry)
		for _, yes := range []bool{false, true} {
			t.Run(strings.ReplaceAll(entry, "/", "_")+map[bool]string{false: "/no", true: "/yes"}[yes], func(t *testing.T) {
				f := newFixture(t, fixtureOpts{branch: map[string]string{"src/x.go": "package x\n", file: "x\n"}})
				c := &confirmer{answer: yes}
				co := f.checkout(RunGit)
				co.Confirm = c.confirm
				got, err := co.Run(t.Context())
				if len(c.asked) != 1 || !slices.Equal(c.asked[0], []string{file}) {
					t.Fatalf("asked %v", c.asked)
				}
				_, statErr := os.Stat(filepath.Join(f.parent, "a.26zfnuxm"))
				if yes {
					if err != nil || statErr != nil || got.Path == "" {
						t.Fatalf("%v %v", err, statErr)
					}
					return
				}
				if _ = asRefusal(t, err); statErr == nil {
					t.Fatal("a refused checkout created the worktree")
				}
			})
		}
	}
}

func TestCheckoutRefusesFlaggedFilesWithoutForms(t *testing.T) {
	f := newFixture(t, fixtureOpts{branch: map[string]string{".mcp.json": "{}\n"}})
	_, err := f.checkout(RunGit).Run(t.Context())
	if r := asRefusal(t, err); !strings.Contains(r.Why, ".mcp.json") {
		t.Fatalf("%+v", r)
	}
	if _, err := os.Stat(filepath.Join(f.parent, "a.26zfnuxm")); err == nil {
		t.Fatal("created")
	}
}

func TestCheckoutUnchangedFlaggedFilesAreNotAsked(t *testing.T) {
	f := newFixture(t, fixtureOpts{branch: map[string]string{"src/x.go": "package x\n"}})
	put(t, f.ws, ".mcp.json", "{}\n", 0o644) // untracked in the workspace: only the branch's change counts
	if _, err := f.checkout(RunGit).Run(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestCheckoutRunsNoHook(t *testing.T) {
	f := newFixture(t, fixtureOpts{hooks: true, branch: map[string]string{".husky/post-checkout": "#!/bin/sh\ntouch '" + "MARK" + "'\n"}})
	// the sanity check: without the guard, the clone's hooks do run
	gitIn(t, f.ws, "update-ref", "refs/probe", "HEAD")
	if _, err := os.Stat(f.marker + ".reference-transaction"); err != nil {
		t.Fatalf("the fixture's hook does not run: %v", err)
	}
	_ = os.Remove(f.marker + ".reference-transaction")
	c := &confirmer{answer: true}
	co := f.checkout(RunGit)
	co.Confirm = c.confirm
	if _, err := co.Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"post-checkout", "reference-transaction"} {
		if _, err := os.Stat(f.marker + "." + h); err == nil {
			t.Fatalf("%s ran", h)
		}
	}
	if len(c.asked) != 1 || !slices.Equal(c.asked[0], []string{".husky/post-checkout"}) {
		t.Fatalf("%v", c.asked)
	}
}

func TestCheckoutFlagsTheRepositorysHooksPath(t *testing.T) {
	f := newFixture(t, fixtureOpts{hooksDir: "ci/hooks", branch: map[string]string{"ci/hooks/pre-commit": "#!/bin/sh\n"}})
	c := &confirmer{answer: false}
	co := f.checkout(RunGit)
	co.Confirm = c.confirm
	_, err := co.Run(t.Context())
	_ = asRefusal(t, err)
	if len(c.asked) != 1 || !slices.Equal(c.asked[0], []string{"ci/hooks/pre-commit"}) {
		t.Fatalf("%v", c.asked)
	}
}

func TestCheckoutRefusesAnotherRepository(t *testing.T) {
	f := newFixture(t, fixtureOpts{url: "git@github.com:Smana/b.git", branch: map[string]string{"x": "x"}})
	_, err := f.checkout(RunGit).Run(t.Context())
	if r := asRefusal(t, err); r.Fix != "gh repo clone Smana/a" {
		t.Fatalf("%+v", r)
	}
	// not a repository at all
	co := f.checkout(RunGit)
	co.Workspace = t.TempDir()
	_, err = co.Run(t.Context())
	if r := asRefusal(t, err); r.Fix != "gh repo clone Smana/a" {
		t.Fatalf("%+v", r)
	}
}

func TestCheckoutOriginForms(t *testing.T) {
	for _, url := range []string{
		"git@github.com:Smana/a", "git@github.com:Smana/a.git",
		"https://github.com/Smana/a", "https://github.com/smana/A.git",
	} {
		t.Run(url, func(t *testing.T) {
			f := newFixture(t, fixtureOpts{url: url, branch: map[string]string{"x": "x"}})
			if _, err := f.checkout(RunGit).Run(t.Context()); err != nil {
				t.Fatal(err)
			}
		})
	}
	for _, url := range []string{
		"https://evil.example/Smana/a", "git@github.com:Smana/a/extra", "https://github.com/Smana/ab",
		"https://github.com.evil.example/Smana/a", "ssh://git@github.com/Smana/a.git",
	} {
		t.Run("refuses "+url, func(t *testing.T) {
			f := newFixture(t, fixtureOpts{url: url, branch: map[string]string{"x": "x"}})
			_, err := f.checkout(RunGit).Run(t.Context())
			_ = asRefusal(t, err)
		})
	}
}

func TestCheckoutBranchGone(t *testing.T) {
	f := newFixture(t, fixtureOpts{})
	_, err := f.checkout(RunGit).Run(t.Context())
	if r := asRefusal(t, err); !strings.Contains(r.Why, "https://github.com/Smana/a/pull/7") {
		t.Fatalf("%+v", r)
	}
}

func TestCheckoutHostileFileNames(t *testing.T) {
	f := newFixture(t, fixtureOpts{branch: map[string]string{".husky/x\n\u202eevil": "x\n", "sub\n\u202e/.mcp.json": "{}\n"}})
	c := &confirmer{answer: false}
	co := f.checkout(RunGit)
	co.Confirm = c.confirm
	_, err := co.Run(t.Context())
	_ = asRefusal(t, err)
	if len(c.asked) != 1 || len(c.asked[0]) != 2 {
		t.Fatalf("%q", c.asked)
	}
	for _, p := range c.asked[0] {
		if strings.ContainsAny(p, "\n\u202e") {
			t.Fatalf("%q", p)
		}
	}
}

func TestCheckoutRefusesAnExistingPath(t *testing.T) {
	f := newFixture(t, fixtureOpts{branch: map[string]string{"x": "x"}})
	other := t.TempDir()
	if err := os.Symlink(other, filepath.Join(f.parent, "a.26zfnuxm")); err != nil {
		t.Fatal(err)
	}
	_, err := f.checkout(RunGit).Run(t.Context())
	_ = asRefusal(t, err)
	if es, _ := os.ReadDir(other); len(es) != 0 {
		t.Fatal("wrote through a symlink")
	}
}

func TestCheckoutRefusesAHostileRoomOrRepository(t *testing.T) {
	f := newFixture(t, fixtureOpts{branch: map[string]string{"x": "x"}})
	for _, room := range []string{"../x", "26zfnuxm/..", "-b", "", "ABCDEFGH"} {
		co := f.checkout(RunGit)
		co.Room = room
		if _, err := co.Run(t.Context()); err == nil {
			t.Fatalf("room %q", room)
		}
	}
	co := f.checkout(RunGit)
	co.Repository = "--upload-pack=x/y"
	if _, err := co.Run(t.Context()); err == nil {
		t.Fatal("repository")
	}
}

// sync with roomctl.Agents (PR 5): every project MCP snippet path `roomctl setup` writes.
func TestFlaggedCoversEveryProjectSnippet(t *testing.T) {
	for _, p := range []string{".mcp.json", "opencode.json", ".codex/config.toml", ".gemini/settings.json", ".cursor/mcp.json", ".github/mcp.json"} {
		if !slices.Contains(flagged, p) {
			t.Errorf("%s is not flagged", p)
		}
	}
}

// checkoutCall calls room_checkout and answers what the relay asks the client: roots with ws, and
// the form with answers (nil: no form is expected). It returns the result and the forms it saw.
func checkoutCall(t *testing.T, s session, room, ws string, answers map[string]any) (map[string]any, []map[string]any) {
	t.Helper()
	s.send(t, map[string]any{"jsonrpc": "2.0", "id": 9, "method": "tools/call", "params": map[string]any{"name": toolRoomCheckout, "arguments": map[string]any{"room": room}}})
	var forms []map[string]any
	for {
		m := s.next(t)
		switch {
		case m["method"] == "roots/list":
			s.send(t, map[string]any{"jsonrpc": "2.0", "id": m["id"], "result": map[string]any{"roots": []any{map[string]any{"uri": "file://" + ws}}}})
		case m["method"] == "elicitation/create":
			forms = append(forms, m)
			if answers == nil {
				t.Fatalf("no form expected: %v", m)
			}
			s.send(t, map[string]any{"jsonrpc": "2.0", "id": m["id"], "result": answers})
		case m["id"] == 9.0 && m["method"] == nil:
			return m, forms
		}
	}
}

func TestRelayRoomCheckout(t *testing.T) {
	f := newFixture(t, fixtureOpts{branch: map[string]string{".mcp.json": "{}\n"}})
	b := newMCPBroker(t)
	b.roomsFn = func(int, map[string]any) []map[string]any {
		return []map[string]any{row("26zfnuxm", func(m map[string]any) { m["repository"], m["pr"] = "Smana/a", 7 })}
	}
	caps := map[string]any{"roots": map[string]any{}, "elicitation": map[string]any{"form": map[string]any{}}}
	start := func(caps map[string]any) session {
		s := relayTo(t, b, tok)
		s.request(t, 1, "initialize", map[string]any{"capabilities": caps})
		return s
	}
	t.Run("a declined form creates nothing", func(t *testing.T) {
		res, forms := checkoutCall(t, start(caps), "26zfnuxm", f.ws, map[string]any{"action": "accept", "content": map[string]any{"create": false}})
		text, _, isErr := resultOf(res)
		if !isErr || !strings.Contains(text, "did not confirm") || len(forms) != 1 || !strings.Contains(fmt.Sprint(forms[0]), ".mcp.json") {
			t.Fatalf("%q %v", text, forms)
		}
		if _, err := os.Stat(filepath.Join(f.parent, "a.26zfnuxm")); err == nil {
			t.Fatal("created")
		}
	})
	t.Run("a client without forms is refused", func(t *testing.T) {
		text, _, isErr := resultOf(first(checkoutCall(t, start(map[string]any{"roots": map[string]any{}}), "26zfnuxm", f.ws, nil)))
		if !isErr || !strings.Contains(text, ".mcp.json") || !strings.Contains(text, "cannot ask") {
			t.Fatalf("%q", text)
		}
	})
	t.Run("an unknown room", func(t *testing.T) {
		text, _, isErr := resultOf(first(checkoutCall(t, start(caps), "aaaaaaaa", f.ws, nil)))
		if !isErr || !strings.Contains(text, "unknown_room") {
			t.Fatalf("%q", text)
		}
	})
	t.Run("a confirmed form creates the worktree", func(t *testing.T) {
		res, _ := checkoutCall(t, start(caps), "26zfnuxm", f.ws, map[string]any{"action": "accept", "content": map[string]any{"create": true}})
		_, sc, isErr := resultOf(res)
		if isErr || sc["path"] != filepath.Join(f.parent, "a.26zfnuxm") || sc["branch"] != "agent/26zfnuxm" || sc["reused"] != false {
			t.Fatalf("%v", sc)
		}
	})
}
