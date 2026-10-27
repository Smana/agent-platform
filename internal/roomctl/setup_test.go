// SPDX-License-Identifier: Apache-2.0

package roomctl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite testdata/setup/*.txt from the current output")

func noRun(t *testing.T) func(context.Context, []string) error {
	return func(_ context.Context, argv []string) error {
		t.Errorf("ran %v, want nothing", argv)
		return nil
	}
}

func found(string) (string, error)  { return "/usr/bin/x", nil }
func absent(string) (string, error) { return "", errors.New("not found") }

func filesUnder(t *testing.T, roots ...string) []string {
	t.Helper()
	var out []string
	for _, r := range roots {
		_ = filepath.WalkDir(r, func(p string, d fs.DirEntry, err error) error {
			if err == nil && !d.IsDir() {
				out = append(out, p)
			}
			return nil
		})
	}
	return out
}

func TestSetupPrintGolden(t *testing.T) {
	for name, a := range Agents {
		for _, project := range []bool{false, true} {
			file := name
			if project {
				file += "-project"
			}
			t.Run(file, func(t *testing.T) {
				home, dir := t.TempDir(), t.TempDir()
				var out bytes.Buffer
				// Home and Dir are fresh: the golden names the logical paths, so the fake roots
				// are the documented ones and writes would show in the temp dirs checked below.
				s := Setup{Agent: a, Project: project, Print: true, Home: "/home/dev", Dir: "/work/repo", Cmd: "roomctl",
					Version: "v0", LookPath: found, Run: noRun(t), Out: &out}
				if err := s.Do(context.Background()); err != nil {
					t.Fatal(err)
				}
				golden := filepath.Join("testdata", "setup", file+".txt")
				if *updateGolden {
					if err := os.WriteFile(golden, out.Bytes(), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				want, err := os.ReadFile(golden)
				if err != nil {
					t.Fatal(err)
				}
				if out.String() != string(want) {
					t.Errorf("--print output:\n%s\nwant:\n%s", out.String(), want)
				}
				if got := filesUnder(t, home, dir); len(got) != 0 {
					t.Errorf("wrote %v", got)
				}
			})
		}
	}
}

func TestSetupPrintWritesNothing(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	s := Setup{Agent: Agents["claude"], Print: true, Home: home, Dir: dir, Cmd: "roomctl", LookPath: found, Run: noRun(t), Out: &bytes.Buffer{}}
	if err := s.Do(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := filesUnder(t, home, dir); len(got) != 0 {
		t.Errorf("--print wrote %v", got)
	}
}

func TestSetupRegistersThroughTheCLI(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	var ran [][]string
	var out bytes.Buffer
	s := Setup{Agent: Agents["claude"], Home: home, Dir: dir, Cmd: "roomctl", Version: "v1", LookPath: found, Out: &out,
		Run: func(_ context.Context, argv []string) error { ran = append(ran, argv); return nil }}
	if err := s.Do(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"claude", "mcp", "add", "--scope", "user", "factory", "--", "roomctl", "mcp"}
	if len(ran) != 1 || !slices.Equal(ran[0], want) {
		t.Fatalf("ran %v, want %v", ran, want)
	}
	if _, err := os.Stat(filepath.Join(home, ".claude", "skills", "factory-handoff", "SKILL.md")); err != nil {
		t.Errorf("the skill is not in <home>/.claude/skills: %v", err)
	}
	if got := filesUnder(t, dir); len(got) != 0 {
		t.Errorf("a user-level setup wrote into the project: %v", got)
	}
}

func TestSetupProjectInstallsInTheProject(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	var ran [][]string
	s := Setup{Agent: Agents["claude"], Project: true, Home: home, Dir: dir, Cmd: "roomctl", Version: "v1", LookPath: found, Out: &bytes.Buffer{},
		Run: func(_ context.Context, argv []string) error { ran = append(ran, argv); return nil }}
	if err := s.Do(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(ran) != 1 || ran[0][4] != "project" {
		t.Errorf("ran %v, want --scope project", ran)
	}
	if _, err := os.Stat(filepath.Join(dir, ".claude", "skills", "factory-handoff", "SKILL.md")); err != nil {
		t.Errorf("the skill is not in <dir>/.claude/skills: %v", err)
	}
	if got := filesUnder(t, home); len(got) != 0 {
		t.Errorf("a project setup wrote into home: %v", got)
	}
}

func TestSetupPrintsWhenTheCLIIsMissing(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	var out bytes.Buffer
	s := Setup{Agent: Agents["claude"], Project: true, Home: home, Dir: dir, Cmd: "roomctl", Version: "v1", LookPath: absent, Run: noRun(t), Out: &out}
	if err := s.Do(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"mcpServers"`) || !strings.Contains(out.String(), ".mcp.json") {
		t.Errorf("no snippet in:\n%s", out.String())
	}
}

func TestSetupNeverEditsAConfig(t *testing.T) {
	for name, a := range Agents {
		for _, project := range []bool{false, true} {
			t.Run(name, func(t *testing.T) {
				home, dir := t.TempDir(), t.TempDir()
				s := Setup{Agent: a, Project: project, Home: home, Dir: dir, Cmd: "roomctl", Version: "v1", LookPath: absent, Run: noRun(t), Out: &bytes.Buffer{}}
				if err := s.Do(context.Background()); err != nil {
					t.Fatal(err)
				}
				root := home
				if project {
					root = dir
				}
				skills := filepath.Join(root, strings.TrimPrefix(a.skillDir(project), "~/"))
				command := ""
				if c := a.commandDir(project); c != "" {
					command = filepath.Join(root, strings.TrimPrefix(c, "~/"), "factory.md")
				}
				for _, f := range filesUnder(t, home, dir) {
					if !strings.HasPrefix(f, filepath.Join(skills, "factory-handoff")+string(filepath.Separator)) && f != command {
						t.Errorf("wrote %s, not a skill file or the /factory command", f)
					}
				}
			})
		}
	}
}

func TestSetupRunFailureIsReturned(t *testing.T) {
	s := Setup{Agent: Agents["claude"], Home: t.TempDir(), Dir: t.TempDir(), Cmd: "roomctl", LookPath: found, Out: &bytes.Buffer{},
		Run: func(context.Context, []string) error { return errors.New("boom") }}
	if err := s.Do(context.Background()); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Errorf("err = %v, want boom", err)
	}
}

func TestSetupUnknownClient(t *testing.T) {
	_, err := LookupAgent("vim")
	if err == nil {
		t.Fatal("want an error")
	}
	for _, n := range []string{"claude", "opencode", "codex", "gemini", "cursor", "copilot"} {
		if !strings.Contains(err.Error(), n) {
			t.Errorf("%q does not name %s", err, n)
		}
	}
}

func TestSetupCmdPrefersPATH(t *testing.T) {
	exe := func() (string, error) { return "/opt/bin/roomctl", nil }
	onPath := func(n string) (string, error) { return "/usr/bin/" + n, nil }
	if got := SetupCmd(onPath, exe); got != "roomctl" {
		t.Errorf("on PATH: %q, want roomctl", got)
	}
	if got := SetupCmd(absent, exe); got != "/opt/bin/roomctl" {
		t.Errorf("off PATH: %q, want the absolute executable", got)
	}
}

func TestSetupCopilotProjectIsWhatTheCLIReads(t *testing.T) {
	a := Agents["copilot"]
	if a.SnippetProjectPath != ".github/mcp.json" {
		t.Errorf("project path %q, want .github/mcp.json", a.SnippetProjectPath)
	}
	if a.SnippetProject != a.Snippet || !strings.Contains(a.SnippetProject, `"mcpServers"`) {
		t.Errorf("project snippet is not the CLI's mcpServers shape:\n%s", a.SnippetProject)
	}
}

func TestSetupCursorSnippetIsStdio(t *testing.T) {
	if !strings.Contains(Agents["cursor"].Snippet, `"type": "stdio"`) {
		t.Errorf("cursor snippet lacks type stdio:\n%s", Agents["cursor"].Snippet)
	}
}

func TestSetupSkillDirsOfCodex(t *testing.T) {
	a := Agents["codex"]
	if a.SkillDir != "~/.agents/skills" || a.SkillProjectDir != ".agents/skills" {
		t.Errorf("codex skill dirs %q, %q", a.SkillDir, a.SkillProjectDir)
	}
}

func TestSetupGeminiHintsAtTrust(t *testing.T) {
	var out bytes.Buffer
	s := Setup{Agent: Agents["gemini"], Home: t.TempDir(), Dir: t.TempDir(), Cmd: "roomctl", LookPath: absent, Run: noRun(t), Out: &out}
	if err := s.Do(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "trusted folder") {
		t.Errorf("no trust hint in:\n%s", out.String())
	}
}

func TestSetupWarnsOnAnAbsolutePathInAProjectSnippet(t *testing.T) {
	for _, tc := range []struct {
		cmd     string
		project bool
		warn    bool
	}{{"/opt/bin/roomctl", true, true}, {"/opt/bin/roomctl", false, false}, {"roomctl", true, false}} {
		var out bytes.Buffer
		s := Setup{Agent: Agents["cursor"], Project: tc.project, Home: t.TempDir(), Dir: t.TempDir(), Cmd: tc.cmd, LookPath: absent, Run: noRun(t), Out: &out}
		if err := s.Do(context.Background()); err != nil {
			t.Fatal(err)
		}
		if got := strings.Contains(out.String(), "teammates"); got != tc.warn {
			t.Errorf("%+v: warning = %v in:\n%s", tc, got, out.String())
		}
	}
}

func TestSetupRefusesASymlinkedSkillDir(t *testing.T) {
	home, dir, elsewhere := t.TempDir(), t.TempDir(), t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".cursor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(elsewhere, filepath.Join(home, ".cursor", "skills")); err != nil {
		t.Fatal(err)
	}
	s := Setup{Agent: Agents["cursor"], Home: home, Dir: dir, Cmd: "roomctl", LookPath: absent, Run: noRun(t), Out: &bytes.Buffer{}}
	err := s.Do(context.Background())
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("err = %v, want a symlink refusal", err)
	}
	if got := filesUnder(t, elsewhere); len(got) != 0 {
		t.Errorf("wrote through the symlink: %v", got)
	}
}

func TestSetupTwiceSucceedsAndRefreshesTheSkill(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	adds := 0
	run := func(context.Context, []string) error {
		if adds++; adds > 1 {
			return errors.New("exit status 1: MCP server factory already exists in user config") // claude, verbatim
		}
		return nil
	}
	s := Setup{Agent: Agents["claude"], Home: home, Dir: dir, Cmd: "roomctl", Version: "v1", LookPath: found, Out: &bytes.Buffer{}, Run: run}
	if err := s.Do(context.Background()); err != nil {
		t.Fatal(err)
	}
	skillMD := filepath.Join(home, ".claude", "skills", "factory-handoff", "SKILL.md")
	if err := os.WriteFile(skillMD, []byte("stale"), 0o644); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	s.Out = &out
	if err := s.Do(context.Background()); err != nil {
		t.Fatalf("second run: %v", err)
	}
	if b, _ := os.ReadFile(skillMD); string(b) == "stale" {
		t.Error("the second run did not refresh the skill")
	}
	if !strings.Contains(out.String(), "already registered") {
		t.Errorf("no already-registered line in:\n%s", out.String())
	}
}

func TestSetupInstallsTheSkillBeforeRegistering(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".claude"), []byte("a file"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := Setup{Agent: Agents["claude"], Home: home, Dir: t.TempDir(), Cmd: "roomctl", LookPath: found, Out: &bytes.Buffer{}, Run: noRun(t)}
	if err := s.Do(context.Background()); err == nil {
		t.Error("want the install to fail")
	}
}

// What the CLI writes, then setup's codemode flag (O9).
func TestSetupOpencodeSnippetIsWhatTheCLIWrites(t *testing.T) {
	want := `{"mcp":{"servers":{"factory":{"type":"local","command":["roomctl","mcp"],"codemode":false,"timeout":{"execution":900000}}}}}`
	got := strings.NewReplacer(" ", "", "\n", "").Replace(strings.ReplaceAll(Agents["opencode"].Snippet, "{CMD}", "roomctl"))
	if got != want {
		t.Errorf("opencode snippet %s, want %s", got, want)
	}
}

func TestSkillDirsWithoutARepositorySkipsTheProjectScope(t *testing.T) {
	for _, d := range SkillDirs("/home/dev", "") {
		if !strings.HasPrefix(d, "/home/dev/") {
			t.Errorf("%s is not under home", d)
		}
	}
	if len(SkillDirs("/home/dev", "/repo")) <= len(SkillDirs("/home/dev", "")) {
		t.Error("a repository adds no project dirs")
	}
}

// O6: the MCP prompts autocomplete as /factory:handoff but typing that is rejected, and only
// /mcp__factory__handoff runs. setup installs a short /factory command, with handoff, status and
// inbox as its subcommands, for the agents that have a command mechanism.
func TestSetupInstallsTheFactoryCommand(t *testing.T) {
	for name, tc := range map[string]struct{ user, project string }{
		"claude":   {".claude/commands", ".claude/commands"},
		"opencode": {".config/opencode/commands", ".opencode/commands"},
	} {
		for _, project := range []bool{false, true} {
			t.Run(name, func(t *testing.T) {
				home, dir := t.TempDir(), t.TempDir()
				s := Setup{Agent: Agents[name], Project: project, Home: home, Dir: dir, Cmd: "roomctl", Version: "v1", LookPath: absent, Run: noRun(t), Out: &bytes.Buffer{}}
				if err := s.Do(context.Background()); err != nil {
					t.Fatal(err)
				}
				root, sub := home, tc.user
				if project {
					root, sub = dir, tc.project
				}
				b, err := os.ReadFile(filepath.Join(root, sub, "factory.md"))
				if err != nil {
					t.Fatal(err)
				}
				for _, w := range []string{"description:", "$ARGUMENTS", "handoff", "status", "inbox", "issue_draft", "issue_file", "room_summary", "rooms_list", "never apply a label"} {
					if !strings.Contains(string(b), w) {
						t.Errorf("the command lacks %q:\n%s", w, b)
					}
				}
			})
		}
	}
	// An agent with no command mechanism gets none.
	home, dir := t.TempDir(), t.TempDir()
	s := Setup{Agent: Agents["codex"], Home: home, Dir: dir, Cmd: "roomctl", Version: "v1", LookPath: absent, Run: noRun(t), Out: &bytes.Buffer{}}
	if err := s.Do(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, f := range filesUnder(t, home, dir) {
		if strings.HasSuffix(f, "factory.md") {
			t.Errorf("codex got %s", f)
		}
	}
}

// O8: OpenCode's background service keeps the config it started with, so sessions attached to it
// have no factory tools although `opencode mcp list` says connected. setup says so, and restarts
// the service only on a yes (a terminal's, or --restart-service), never silently.
func TestSetupOpenCodeServiceRestart(t *testing.T) {
	type rig struct {
		ran      [][]string
		status   string
		statusEr error
		asked    int
	}
	run := func(t *testing.T, r *rig, restart bool, ask func(string) (bool, error)) string {
		t.Helper()
		var out bytes.Buffer
		s := Setup{Agent: Agents["opencode"], Home: t.TempDir(), Dir: t.TempDir(), Cmd: "roomctl", Version: "v1", LookPath: found, Out: &out,
			Run: func(_ context.Context, argv []string) error { r.ran = append(r.ran, argv); return nil },
			Capture: func(_ context.Context, argv []string) (string, error) {
				r.ran = append(r.ran, argv)
				return r.status, r.statusEr
			},
			RestartService: restart, Ask: ask}
		if err := s.Do(context.Background()); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	restarted := func(r *rig) bool {
		return slices.ContainsFunc(r.ran, func(a []string) bool { return slices.Equal(a, []string{"opencode", "service", "restart"}) })
	}
	t.Run("running, no terminal: says it, restarts nothing", func(t *testing.T) {
		r := &rig{status: "http://127.0.0.1:49374\n"}
		out := run(t, r, false, nil)
		if restarted(r) || !strings.Contains(out, "opencode service restart") || !strings.Contains(out, "restart") || !strings.Contains(out, "factory tools") {
			t.Fatalf("restarted=%v\n%s", restarted(r), out)
		}
	})
	t.Run("running, terminal says no", func(t *testing.T) {
		r := &rig{status: "http://127.0.0.1:49374\n"}
		out := run(t, r, false, func(string) (bool, error) { r.asked++; return false, nil })
		if restarted(r) || r.asked != 1 || !strings.Contains(out, "not restarted") {
			t.Fatalf("restarted=%v asked=%d\n%s", restarted(r), r.asked, out)
		}
	})
	t.Run("running, terminal says yes", func(t *testing.T) {
		r := &rig{status: "http://127.0.0.1:49374\n"}
		out := run(t, r, false, func(string) (bool, error) { return true, nil })
		if !restarted(r) || !strings.Contains(out, "restarted") {
			t.Fatalf("restarted=%v\n%s", restarted(r), out)
		}
	})
	t.Run("--restart-service restarts without asking", func(t *testing.T) {
		r := &rig{status: "http://127.0.0.1:49374\n"}
		run(t, r, true, func(string) (bool, error) { r.asked++; return false, nil })
		if !restarted(r) || r.asked != 0 {
			t.Fatalf("restarted=%v asked=%d", restarted(r), r.asked)
		}
	})
	t.Run("not running: nothing said, nothing restarted", func(t *testing.T) {
		r := &rig{status: "not running\n"}
		out := run(t, r, true, nil)
		if restarted(r) || strings.Contains(out, "service") {
			t.Fatalf("restarted=%v\n%s", restarted(r), out)
		}
	})
	t.Run("no service subcommand: the hint only", func(t *testing.T) {
		r := &rig{statusEr: errors.New("unknown command service")}
		out := run(t, r, true, func(string) (bool, error) { return true, nil })
		if restarted(r) || !strings.Contains(out, "opencode service restart") {
			t.Fatalf("restarted=%v\n%s", restarted(r), out)
		}
	})
	t.Run("another client is not asked about it", func(t *testing.T) {
		var out bytes.Buffer
		s := Setup{Agent: Agents["claude"], Home: t.TempDir(), Dir: t.TempDir(), Cmd: "roomctl", Version: "v1", LookPath: found, Out: &out,
			Run:     func(context.Context, []string) error { return nil },
			Capture: func(context.Context, []string) (string, error) { t.Error("captured"); return "", nil }}
		if err := s.Do(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
}

// O9: OpenCode v2 exposes MCP tools through Code Mode by default, and a config with an `execute`
// deny rule turns Code Mode off, hiding every Code Mode server. `"codemode": false` on the server
// keeps its tools on the native list, where the elicitation forms and room_wait also belong.
func TestSetupOpenCodeWritesCodeModeOff(t *testing.T) {
	const existing = `{"theme":"dark","permission":[{"action":"execute","resource":"*","effect":"deny"}],"mcp":{"servers":{"other":{"type":"local","command":["x"]},"factory":{"type":"local","command":["roomctl","mcp"]}}}}`
	for name, project := range map[string]bool{"user": false, "project": true} {
		t.Run(name, func(t *testing.T) {
			home, dir := t.TempDir(), t.TempDir()
			path := filepath.Join(home, ".config", "opencode", "opencode.json")
			if project {
				path = filepath.Join(dir, "opencode.json")
			}
			var out bytes.Buffer
			// `opencode mcp add` writes the file; the stub plays it.
			add := func(context.Context, []string) error {
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					return err
				}
				return os.WriteFile(path, []byte(existing), 0o644)
			}
			s := Setup{Agent: Agents["opencode"], Project: project, Home: home, Dir: dir, Cmd: "roomctl", Version: "v1", LookPath: found, Out: &out, Run: add}
			for range 2 { // a second setup changes nothing more
				if err := s.Do(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			var got map[string]any
			b, _ := os.ReadFile(path)
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatalf("%v\n%s", err, b)
			}
			servers := got["mcp"].(map[string]any)["servers"].(map[string]any)
			if f := servers["factory"].(map[string]any); f["codemode"] != false || f["type"] != "local" || fmt.Sprint(f["timeout"]) != "map[execution:900000]" {
				t.Errorf("factory: %v", f)
			}
			if _, ok := servers["other"]; !ok || got["theme"] != "dark" || got["permission"] == nil {
				t.Errorf("the rest of the file was not kept: %s", b)
			}
			if !strings.Contains(out.String(), "codemode") {
				t.Errorf("setup does not say it:\n%s", &out)
			}
		})
	}
}

// A config with comments is not rewritten; the developer is told what to add.
func TestSetupOpenCodeLeavesACommentedConfig(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".config", "opencode", "opencode.json")
	const commented = "// mine\n{\"mcp\":{\"servers\":{\"factory\":{}}}}\n"
	var out bytes.Buffer
	s := Setup{Agent: Agents["opencode"], Home: home, Dir: t.TempDir(), Cmd: "roomctl", Version: "v1", LookPath: found, Out: &out,
		Run: func(context.Context, []string) error {
			_ = os.MkdirAll(filepath.Dir(path), 0o755)
			return os.WriteFile(path, []byte(commented), 0o644)
		}}
	if err := s.Do(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != commented || !strings.Contains(out.String(), `"codemode": false`) {
		t.Fatalf("%q\n%s", b, &out)
	}
}

// The snippet a developer pastes has it too.
func TestSetupOpenCodeSnippetHasCodeModeOff(t *testing.T) {
	var out bytes.Buffer
	s := Setup{Agent: Agents["opencode"], Home: t.TempDir(), Dir: t.TempDir(), Cmd: "roomctl", Version: "v1", LookPath: absent, Run: noRun(t), Out: &out}
	if err := s.Do(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), `"codemode": false`) || !strings.Contains(out.String(), `"execution": 900000`) {
		t.Fatalf("%s", &out)
	}
}
