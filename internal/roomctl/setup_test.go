// SPDX-License-Identifier: Apache-2.0

package roomctl

import (
	"bytes"
	"context"
	"errors"
	"flag"
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
				for _, f := range filesUnder(t, home, dir) {
					if !strings.HasPrefix(f, filepath.Join(skills, "factory-handoff")+string(filepath.Separator)) {
						t.Errorf("wrote %s, not a skill file", f)
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
