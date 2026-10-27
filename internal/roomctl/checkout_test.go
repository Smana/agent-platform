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

func link(t *testing.T, root, rel, target string) {
	t.Helper()
	if err := os.Symlink(target, filepath.Join(root, rel)); err != nil {
		t.Fatal(err)
	}
}

type fixture struct {
	seed               string // the clone that pushes to origin; on agent/26zfnuxm when there is one
	origin, ws, parent string
	marker             string // the file the clone's hooks touch
}

type fixtureOpts struct {
	branch   map[string]string // files the agent branch adds, nil: no agent branch
	links    map[string]string // symlinks the agent branch adds: path -> target
	main     map[string]string // files main carries
	mainLink map[string]string // symlinks main carries
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
	f.seed = seed
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
	for rel, body := range o.main {
		put(t, seed, rel, body, 0o644)
	}
	for rel, target := range o.mainLink {
		link(t, seed, rel, target)
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
		for rel, target := range o.links {
			link(t, seed, rel, target)
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
		calls = append(calls, strings.Join(args, " "))
		return RunGit(ctx, dir, args...)
	}
	second, err := f.checkout(rec).Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if second.Path != first.Path || !second.Reused {
		t.Fatalf("%+v", second)
	}
	for _, c := range calls {
		if strings.HasPrefix(c, "fetch") || strings.HasPrefix(c, "worktree add") {
			t.Fatalf("a reuse fetched or added: %v", calls)
		}
	}
	if len(calls) == 0 {
		t.Fatal("the recorder saw nothing")
	}
}

// fixtureFile is a file that matches a flagged entry.
func fixtureFile(entry string) string {
	entry = strings.ReplaceAll(entry, "*", "x")
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
		"https://evil.example/Smana/a", "git@github.com:Smana/a/extra", "user@github.com:Smana/a", "https://user@github.com/Smana/a",
		"file:///srv/Smana/a", "ext::sh -c true", "git@github.com:Smana/a/../x", "https://github.com/Smana/ab",
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

// sync with roomctl.Agents (PR 5): every project MCP snippet path and skill folder `roomctl setup` writes.
func TestFlaggedCoversEveryProjectSnippet(t *testing.T) {
	for _, p := range []string{".mcp.json", "opencode.json", ".codex/config.toml", ".gemini/settings.json", ".cursor/mcp.json", ".vscode/mcp.json", ".github/mcp.json",
		".agents/skills/x/SKILL.md", ".claude/skills/x/SKILL.md", ".gemini/skills/x/SKILL.md", ".cursor/skills/x/SKILL.md", ".github/skills/x/SKILL.md"} {
		if !isFlagged(p, flagged) {
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

// A symlink redirects a config directory: git checks it out as a link, so the path strings the
// diff lists name neither the directory nor the file the tool reads.
func TestCheckoutFlagsSymlinks(t *testing.T) {
	for name, tc := range map[string]struct {
		opts fixtureOpts
		want string
	}{
		"a directory link the branch adds": {fixtureOpts{
			branch: map[string]string{"x/settings.json": "{}\n"}, links: map[string]string{".claude": "x"}}, ".claude"},
		"a link anywhere": {fixtureOpts{
			branch: map[string]string{"x/tasks.json": "{}\n"}, links: map[string]string{"sub-link": "x"}}, "sub-link"},
		"a link main already has, whose target the branch changes": {fixtureOpts{
			main: map[string]string{"x/settings.json": "{}\n"}, mainLink: map[string]string{".claude": "x"},
			branch: map[string]string{"x/settings.json": "{\"hooks\":1}\n"}}, ".claude"},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, tc.opts)
			c := &confirmer{answer: false}
			co := f.checkout(RunGit)
			co.Confirm = c.confirm
			_, err := co.Run(t.Context())
			_ = asRefusal(t, err)
			if len(c.asked) != 1 || !slices.Contains(c.asked[0], tc.want) {
				t.Fatalf("asked %q, want %q among them", c.asked, tc.want)
			}
		})
	}
}

func TestIsFlagged(t *testing.T) {
	for p, want := range map[string]bool{
		".claude": true, "sub/.claude": true, ".vscode": true, ".github": true, ".claude/skills/a.md": true,
		".claude/agents/a.md": true, ".devcontainer/devcontainer.json": true, ".vscode/settings.json": true,
		".vscode/mcp.json": true, ".github/mcp.json": true, "sub/.MCP.json": true,
		".cursor/hooks.json": true, ".claude/hooks/pre.sh": true, ".opencode/tool/x.ts": true, ".opencode/plugin/x.ts": true,
		".agents/skills/a/SKILL.md": true, ".gemini/skills/a/SKILL.md": true, ".cursor/skills/a/SKILL.md": true, ".github/skills/a/SKILL.md": true,
		".codex/prompts/x.md": true, ".gitmodules": true, ".lfsconfig": true, "sub/.CLAUDE/x": true,
		"docs/café.md": true, ".huſky/x": true, "miſe.toml": true, ".c\u200claude/x": true,
		".mise.local.toml": true, "mise.test.toml": true, ".mise.dev.toml": true, ".mise/config.toml": true, "mise/config.toml": true,
		".config/mise/conf.d/a.toml": true, "lefthook.yaml": true, "lefthook.toml": true, "lefthook-local.yml": true, ".lefthook.json": true,
		".config/lefthook.yml": true, ".devcontainer.json": true, ".github/hooks/pre": true, ".kiro/hooks/x": true, ".amazonq/mcp.json": true,
		".roo/mcp.json": true, ".continue/config.yaml": true, ".zed/settings.json": true, ".qwen/settings.json": true, ".windsurf/x": true,
		"crush.json": true, ".tool-versions": true, "sub/lefthook.yml": true,
		"mise.md": false, "lefthook": false, "src/mise.go": false, ".github/workflows/ci.yml": false, "src/claude.go": false, "README.md": false, ".claudex": false,
	} {
		if got := isFlagged(p, flagged); got != want {
			t.Errorf("isFlagged(%q) = %v", p, got)
		}
	}
}

func TestCheckoutRefusesSeveralOriginURLs(t *testing.T) {
	f := newFixture(t, fixtureOpts{branch: map[string]string{"x": "x"}})
	gitIn(t, f.ws, "config", "--add", "remote.origin.url", "git@github.com:Smana/a")
	_, err := f.checkout(RunGit).Run(t.Context())
	_ = asRefusal(t, err)
}

// A content filter the developer has (Git LFS) runs on checkout and may reach a host the branch's
// .lfsconfig names: it does not run.
func TestCheckoutRunsNoSmudgeFilter(t *testing.T) {
	f := newFixture(t, fixtureOpts{branch: map[string]string{".gitattributes": "*.bin filter=lfs\n", "a.bin": "x\n"}})
	gitIn(t, f.ws, "config", "filter.lfs.smudge", "touch '"+f.marker+".smudge'; cat")
	if _, err := f.checkout(RunGit).Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(f.marker + ".smudge"); err == nil {
		t.Fatal("the smudge filter ran")
	}
}

func TestCheckoutReadsTheEffectiveHooksPath(t *testing.T) {
	global := filepath.Join(t.TempDir(), "gitconfig")
	put(t, filepath.Dir(global), "gitconfig", "[core]\n\thooksPath = ci/h\n", 0o644)
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	f := newFixture(t, fixtureOpts{branch: map[string]string{"ci/h/pre-commit": "#!/bin/sh\n"}})
	c := &confirmer{answer: false}
	co := f.checkout(RunGit)
	co.Confirm = c.confirm
	_, err := co.Run(t.Context())
	_ = asRefusal(t, err)
	if len(c.asked) != 1 || !slices.Equal(c.asked[0], []string{"ci/h/pre-commit"}) {
		t.Fatalf("%v", c.asked)
	}
}

func TestCheckoutRecreatesADeletedWorktree(t *testing.T) {
	f := newFixture(t, fixtureOpts{branch: map[string]string{"x": "x"}})
	first, err := f.checkout(RunGit).Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(first.Path); err != nil {
		t.Fatal(err)
	}
	got, err := f.checkout(RunGit).Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(got.Path); err != nil || !st.IsDir() || got.Reused {
		t.Fatalf("%+v %v", got, err)
	}
}

// The path is checked again just before the add: one that appears while the developer decides is
// refused, and the add never runs.
func TestCheckoutChecksThePathAgainBeforeTheAdd(t *testing.T) {
	f := newFixture(t, fixtureOpts{branch: map[string]string{".mcp.json": "{}\n"}})
	target := t.TempDir()
	co := f.checkout(RunGit)
	co.Confirm = func(context.Context, []string) (bool, error) {
		return true, os.Symlink(target, filepath.Join(f.parent, "a.26zfnuxm")) // during the form
	}
	_, err := co.Run(t.Context())
	_ = asRefusal(t, err)
	if es, _ := os.ReadDir(target); len(es) != 0 {
		t.Fatal("wrote through the link")
	}
}

// Only a real miss is unknown_room: a broker that cannot answer says so.
func TestRelayRoomCheckoutReportsBrokerErrors(t *testing.T) {
	b := newMCPBroker(t)
	b.status = 500
	s := relayTo(t, b, tok)
	s.request(t, 1, "initialize", map[string]any{"capabilities": map[string]any{}})
	text, _, isErr := resultOf(first(checkoutCall(t, s, "26zfnuxm", t.TempDir(), nil)))
	if !isErr || strings.Contains(text, "unknown_room") || !strings.Contains(text, "not_checked_out") {
		t.Fatalf("%q", text)
	}
}

// advance pushes a new commit to agent/26zfnuxm from the seed, and fetches it into the workspace.
func (f fixture) advance(t *testing.T, files map[string]string) string {
	t.Helper()
	for rel, body := range files {
		put(t, f.seed, rel, body, 0o644)
	}
	gitIn(t, f.seed, "add", "-A")
	gitIn(t, f.seed, "commit", "-q", "-m", "later")
	gitIn(t, f.seed, "push", "-q", "origin", "agent/26zfnuxm")
	gitIn(t, f.ws, "fetch", "-q", "origin", "+refs/heads/agent/26zfnuxm:refs/remotes/origin/agent/26zfnuxm")
	return gitIn(t, f.seed, "rev-parse", "HEAD")
}

// The commit the developer confirms is the commit that is checked out: a fetch while the form is
// open moves the remote ref, and the worktree still gets the one that was checked.
func TestCheckoutCreatesTheCommitThatWasChecked(t *testing.T) {
	f := newFixture(t, fixtureOpts{branch: map[string]string{"mise.toml": "[tasks]\n"}})
	checked := gitIn(t, f.seed, "rev-parse", "HEAD")
	co := f.checkout(RunGit)
	co.Confirm = func(context.Context, []string) (bool, error) {
		f.advance(t, map[string]string{".claude/settings.json": "{\"hooks\":1}\n"}) // what a later fetch brings
		return true, nil
	}
	got, err := co.Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if head := gitIn(t, got.Path, "rev-parse", "HEAD"); head != checked {
		t.Fatalf("checked out %s, confirmed %s", head, checked)
	}
	if _, err := os.Stat(filepath.Join(got.Path, ".claude", "settings.json")); err == nil {
		t.Fatal("the unconfirmed file is in the worktree")
	}
	if up := gitIn(t, got.Path, "rev-parse", "--abbrev-ref", "agent/26zfnuxm@{upstream}"); up != "origin/agent/26zfnuxm" {
		t.Fatalf("upstream %q", up)
	}
}

// A local branch that is not the commit that was checked is never used silently.
func TestCheckoutRefusesAStaleLocalBranch(t *testing.T) {
	f := newFixture(t, fixtureOpts{branch: map[string]string{"x": "x"}})
	gitIn(t, f.ws, "branch", "agent/26zfnuxm", "main")
	_, err := f.checkout(RunGit).Run(t.Context())
	r := asRefusal(t, err)
	if !strings.Contains(r.Error(), "agent/26zfnuxm") || !strings.Contains(r.Error(), "git branch") {
		t.Fatalf("%v", r)
	}
	if _, err := os.Stat(filepath.Join(f.parent, "a.26zfnuxm")); err == nil {
		t.Fatal("created")
	}
	// the same commit is fine
	sha := gitIn(t, f.seed, "rev-parse", "HEAD")
	gitIn(t, f.ws, "branch", "-f", "agent/26zfnuxm", sha)
	got, err := f.checkout(RunGit).Run(t.Context())
	if err != nil || gitIn(t, got.Path, "rev-parse", "HEAD") != sha {
		t.Fatalf("%+v %v", got, err)
	}
}

// failAfter runs the real git, then fails the call that starts with prefix.
func failAfter(prefix string, then func(args []string), err error) Git {
	return func(ctx context.Context, dir string, args ...string) (string, error) {
		out, e := RunGit(ctx, dir, args...)
		if strings.HasPrefix(strings.Join(args, " "), prefix) {
			if then != nil {
				then(args)
			}
			return out, err
		}
		return out, e
	}
}

// A worktree add that ends badly (a cancellation kills git) leaves nothing half-made behind.
func TestCheckoutRemovesAWorktreeThatWasNotFinished(t *testing.T) {
	f := newFixture(t, fixtureOpts{branch: map[string]string{"x": "x"}})
	_, err := f.checkout(failAfter("worktree add", nil, context.Canceled)).Run(t.Context())
	if err == nil {
		t.Fatal("expected the failure")
	}
	if _, err := os.Stat(filepath.Join(f.parent, "a.26zfnuxm")); err == nil {
		t.Fatal("a half-made worktree is left")
	}
	if list := gitIn(t, f.ws, "worktree", "list", "--porcelain"); strings.Contains(list, "a.26zfnuxm") {
		t.Fatalf("still registered: %s", list)
	}
	if got, err := f.checkout(RunGit).Run(t.Context()); err != nil || got.Reused {
		t.Fatalf("%+v %v", got, err)
	}
}

// What the add made must be the commit that was checked, or it is removed.
func TestCheckoutRemovesAWorktreeAtAnotherCommit(t *testing.T) {
	f := newFixture(t, fixtureOpts{branch: map[string]string{"x": "x"}})
	other := gitIn(t, f.ws, "rev-parse", "main")
	g := failAfter("worktree add", func(args []string) {
		gitIn(t, args[len(args)-2], "reset", "-q", "--hard", other) // the new path is the second to last argument
	}, nil)
	_, err := f.checkout(g).Run(t.Context())
	if err == nil || !strings.Contains(err.Error(), "not the commit") {
		t.Fatalf("%v", err)
	}
	if _, err := os.Stat(filepath.Join(f.parent, "a.26zfnuxm")); err == nil {
		t.Fatal("left")
	}
}

// A worktree git is still initialising is not a worktree to reuse.
func TestCheckoutDoesNotReuseAnInitializingWorktree(t *testing.T) {
	f := newFixture(t, fixtureOpts{branch: map[string]string{"x": "x"}})
	first, err := f.checkout(RunGit).Run(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	gitIn(t, f.ws, "worktree", "lock", "--reason", "initializing", first.Path)
	got, err := f.checkout(RunGit).Run(t.Context())
	if err != nil || got.Reused {
		t.Fatalf("%+v %v", got, err)
	}
}

// A link main already has, in a folder and in another case, whose target the branch changes.
func TestCheckoutFlagsAnExistingLinkInAnyFolderAndCase(t *testing.T) {
	f := newFixture(t, fixtureOpts{
		main: map[string]string{"x/settings.json": "{}\n", "sub/keep": "k\n"}, mainLink: map[string]string{"sub/.Claude": "../x"},
		branch: map[string]string{"x/settings.json": "{\"hooks\":1}\n"}})
	c := &confirmer{answer: false}
	co := f.checkout(RunGit)
	co.Confirm = c.confirm
	_, err := co.Run(t.Context())
	_ = asRefusal(t, err)
	if len(c.asked) != 1 || !slices.Contains(c.asked[0], "sub/.Claude") {
		t.Fatalf("%q", c.asked)
	}
}

// A flagged file the branch carries unchanged, which HEAD has since fixed, is still asked about.
func TestCheckoutDiffsAgainstHeadToo(t *testing.T) {
	f := newFixture(t, fixtureOpts{main: map[string]string{".mcp.json": "{}\n"}, branch: map[string]string{"x": "x"}})
	put(t, f.ws, ".mcp.json", "{\"fixed\":1}\n", 0o644)
	gitIn(t, f.ws, "commit", "-q", "-am", "fix the MCP config")
	c := &confirmer{answer: false}
	co := f.checkout(RunGit)
	co.Confirm = c.confirm
	_, err := co.Run(t.Context())
	_ = asRefusal(t, err)
	if len(c.asked) != 1 || !slices.Contains(c.asked[0], ".mcp.json") {
		t.Fatalf("%q", c.asked)
	}
}

func TestCodeSpan(t *testing.T) {
	for in, want := range map[string]string{
		".mcp.json":                 "`.mcp.json`",
		"a`b":                       "`a'b`",
		"[x](https://evil.example)": "`[x](https://evil.example)`",
		strings.Repeat("a", 300):    "`" + strings.Repeat("a", 117) + "...`",
	} {
		if got := CodeSpan(in); got != want {
			t.Errorf("CodeSpan(%q) = %q", in, got)
		}
	}
}

func TestRelayCheckoutFormShowsNamesInCodeSpans(t *testing.T) {
	f := newFixture(t, fixtureOpts{branch: map[string]string{".mcp.json": "{}\n", "sub/.claude/x`[ok](https:/e.example)": "x\n"}})
	b := newMCPBroker(t)
	b.roomsFn = func(int, map[string]any) []map[string]any {
		return []map[string]any{row("26zfnuxm", func(m map[string]any) { m["repository"], m["pr"] = "Smana/a", 7 })}
	}
	s := relayTo(t, b, tok)
	s.request(t, 1, "initialize", map[string]any{"capabilities": map[string]any{"roots": map[string]any{}, "elicitation": map[string]any{}}})
	_, forms := checkoutCall(t, s, "26zfnuxm", f.ws, map[string]any{"action": "decline"})
	msg := fmt.Sprint(forms[0]["params"].(map[string]any)["message"])
	if !strings.Contains(msg, "`.mcp.json`") || !strings.Contains(msg, "`sub/.claude/x'[ok](https:/e.example)`") {
		t.Fatalf("%q", msg)
	}
}

// A cancelled call takes back its form: the client is told, and stops asking the developer.
func TestRelayCancellingCheckoutCancelsItsForm(t *testing.T) {
	f := newFixture(t, fixtureOpts{branch: map[string]string{".mcp.json": "{}\n"}})
	b := newMCPBroker(t)
	b.roomsFn = func(int, map[string]any) []map[string]any {
		return []map[string]any{row("26zfnuxm", func(m map[string]any) { m["repository"], m["pr"] = "Smana/a", 7 })}
	}
	s := relayTo(t, b, tok)
	s.request(t, 1, "initialize", map[string]any{"capabilities": map[string]any{"roots": map[string]any{}, "elicitation": map[string]any{}}})
	s.send(t, map[string]any{"jsonrpc": "2.0", "id": 9, "method": "tools/call", "params": map[string]any{"name": toolRoomCheckout, "arguments": map[string]any{"room": "26zfnuxm"}}})
	var form map[string]any
	for form == nil {
		m := s.next(t)
		switch m["method"] {
		case "roots/list":
			s.send(t, map[string]any{"jsonrpc": "2.0", "id": m["id"], "result": map[string]any{"roots": []any{map[string]any{"uri": "file://" + f.ws}}}})
		case "elicitation/create":
			form = m
		}
	}
	cancelOf(s, t, 9)
	m := s.next(t)
	p, _ := m["params"].(map[string]any)
	if m["method"] != "notifications/cancelled" || p["requestId"] != form["id"] {
		t.Fatalf("%v", m)
	}
}

// What a developer commits on agent/<room> while the form is open is theirs: it survives the
// call, whether the call is cancelled or goes on.
func TestCheckoutNeverDeletesABranchItDidNotCreate(t *testing.T) {
	for name, cancelled := range map[string]bool{"cancelled": true, "not cancelled": false} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t, fixtureOpts{branch: map[string]string{".mcp.json": "{}\n"}})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var mine string
			co := f.checkout(RunGit)
			co.Confirm = func(context.Context, []string) (bool, error) {
				gitIn(t, f.ws, "switch", "-q", "-c", "agent/26zfnuxm")
				put(t, f.ws, "mine.txt", "my work\n", 0o644)
				gitIn(t, f.ws, "add", "mine.txt")
				gitIn(t, f.ws, "commit", "-q", "-m", "my work")
				mine = gitIn(t, f.ws, "rev-parse", "HEAD")
				gitIn(t, f.ws, "switch", "-q", "main")
				if cancelled {
					cancel()
				}
				return true, nil
			}
			if _, err := co.Run(ctx); err == nil {
				t.Fatal("expected a refusal or a cancellation")
			}
			if got := gitIn(t, f.ws, "rev-parse", "refs/heads/agent/26zfnuxm"); got != mine {
				t.Fatalf("the developer's branch is %s, was %s", got, mine)
			}
		})
	}
}

// One checkout of a room at a time: the second is told so, and neither removes the other's worktree.
func TestCheckoutSerialisesPerRoom(t *testing.T) {
	f := newFixture(t, fixtureOpts{branch: map[string]string{".mcp.json": "{}\n"}})
	entered, release := make(chan struct{}), make(chan struct{})
	first := f.checkout(RunGit)
	first.Confirm = func(context.Context, []string) (bool, error) { close(entered); <-release; return true, nil }
	type result struct {
		got CheckedOut
		err error
	}
	done := make(chan result, 1)
	go func() { got, err := first.Run(t.Context()); done <- result{got, err} }()
	<-entered
	second := f.checkout(RunGit)
	second.Confirm = func(context.Context, []string) (bool, error) { return true, nil }
	_, err := second.Run(t.Context())
	if r := asRefusal(t, err); !strings.Contains(r.Error(), "another") {
		t.Fatalf("%v", r)
	}
	close(release)
	res := <-done
	if res.err != nil {
		t.Fatal(res.err)
	}
	if st, err := os.Stat(res.got.Path); err != nil || !st.IsDir() {
		t.Fatalf("the first worktree is gone: %v", err)
	}
	// the lock is released: a third call finds the worktree
	if again, err := f.checkout(RunGit).Run(t.Context()); err != nil || !again.Reused {
		t.Fatalf("%+v %v", again, err)
	}
}

// The real matches come first and in full: padding the list with names that are only asked about
// for their bytes or their mode cannot hide them.
func TestCheckoutShowsRealMatchesFirst(t *testing.T) {
	branch := map[string]string{".claude/settings.json": "{}\n"}
	for i := 0; i < 25; i++ {
		branch[fmt.Sprintf("-\u00e9%02d.txt", i)] = "x\n"
	}
	f := newFixture(t, fixtureOpts{branch: branch})
	c := &confirmer{answer: false}
	co := f.checkout(RunGit)
	co.Confirm = c.confirm
	_, err := co.Run(t.Context())
	_ = asRefusal(t, err)
	got := c.asked[0]
	if got[0] != ".claude/settings.json" || len(got) != 1+maxConfirmed+1 || got[len(got)-1] != "and 5 more" {
		t.Fatalf("%q", got)
	}
}

func TestCheckoutFailsClosedOnAHeadDiffError(t *testing.T) {
	f := newFixture(t, fixtureOpts{branch: map[string]string{"x": "x"}})
	g := func(ctx context.Context, dir string, args ...string) (string, error) {
		if len(args) > 4 && args[0] == "diff" && args[len(args)-2] == "HEAD" {
			return "", errors.New("git diff: fatal: bad object, HEAD is somehow bad")
		}
		return RunGit(ctx, dir, args...)
	}
	if _, err := f.checkout(g).Run(t.Context()); err == nil {
		t.Fatal("a failed HEAD diff must not pass")
	}
}
