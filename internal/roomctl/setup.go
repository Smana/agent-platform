// SPDX-License-Identifier: Apache-2.0

package roomctl

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/Smana/agent-platform/internal/roomctl/skill"
)

// Agent is one coding agent roomctl setup knows: how to register `roomctl mcp` with it, and where
// its Agent Skills live. (Not Client: that is the broker client.) Paths starting with ~/ are
// relative to the home directory, the others to the repository. Add is the agent's own CLI, which
// edits its config; nil means the CLI prompts or does not exist, and setup prints Snippet for the
// developer to paste. {CMD} is the roomctl command.
type Agent struct {
	Name                            string
	Add, AddProject                 []string
	Snippet, SnippetProject         string
	SnippetPath, SnippetProjectPath string
	SkillDir, SkillProjectDir       string
	Note                            string // printed after setup: what the agent still needs
}

const (
	mcpServersJSON = "{\n  \"mcpServers\": {\n    \"factory\": {\n      \"command\": \"{CMD}\",\n      \"args\": [\"mcp\"]\n    }\n  }\n}\n"
	copilotJSON    = "{\n  \"mcpServers\": {\n    \"factory\": {\n      \"type\": \"local\",\n      \"command\": \"{CMD}\",\n      \"args\": [\"mcp\"],\n      \"tools\": [\"*\"]\n    }\n  }\n}\n"
	cursorJSON     = "{\n  \"mcpServers\": {\n    \"factory\": {\n      \"type\": \"stdio\",\n      \"command\": \"{CMD}\",\n      \"args\": [\"mcp\"]\n    }\n  }\n}\n"
	// opencodeJSON is the documented shape (mcp.<name>). opencode 2.0.22's own `mcp add` wrote
	// mcp.servers.<name> instead: a live check settles which one opencode reads.
	opencodeJSON = "{\n  \"mcp\": {\n    \"factory\": {\n      \"type\": \"local\",\n      \"command\": [\"{CMD}\", \"mcp\"]\n    }\n  }\n}\n"
	codexTOML    = "[mcp_servers.factory]\ncommand = \"{CMD}\"\nargs = [\"mcp\"]\n"
)

// Agents is keyed by the roomctl setup argument. Sources, read at build time:
//   - claude: https://code.claude.com/docs/en/mcp (scopes, ~/.claude.json, .mcp.json) and
//     https://code.claude.com/docs/en/skills (~/.claude/skills, .claude/skills); `claude mcp add --help`
//   - opencode: https://opencode.ai/docs/mcp-servers/ and https://opencode.ai/docs/skills/;
//     `opencode mcp add --help` (2.0.22) takes `--global` and `-- cmd args`, so it never prompts; without
//     --global it writes ./opencode.json in the working directory (run in a temp dir)
//   - codex: https://learn.chatgpt.com/docs/extend/mcp?surface=cli (`codex mcp add NAME -- CMD`, user config only;
//     the project's .codex/config.toml is read for trusted projects) and
//     https://learn.chatgpt.com/docs/build-skills (~/.agents/skills, .agents/skills)
//   - gemini: https://geminicli.com/docs/tools/mcp-server/ (`gemini mcp add --scope user|project`)
//     and https://geminicli.com/docs/cli/skills/ (~/.gemini/skills, .gemini/skills)
//   - cursor: https://cursor.com/docs/context/mcp (~/.cursor/mcp.json, .cursor/mcp.json) and
//     https://cursor.com/docs/context/skills (.cursor/skills, ~/.cursor/skills)
//   - copilot: https://docs.github.com/en/copilot/how-tos/copilot-cli/customize-copilot/add-mcp-servers
//     (~/.copilot/mcp-config.json; the CLI reads .github/mcp.json in a project, not VS Code's
//     .vscode/mcp.json, whose key is "servers") and https://docs.github.com/en/copilot/concepts/agents/about-agent-skills
//     (.github/skills, ~/.copilot/skills)
var Agents = map[string]Agent{
	"claude": {Name: "claude",
		Add:        []string{"claude", "mcp", "add", "--scope", "user", "factory", "--", "{CMD}", "mcp"},
		AddProject: []string{"claude", "mcp", "add", "--scope", "project", "factory", "--", "{CMD}", "mcp"},
		Snippet:    mcpServersJSON, SnippetProject: mcpServersJSON,
		SnippetPath: "~/.claude.json", SnippetProjectPath: ".mcp.json",
		SkillDir: "~/.claude/skills", SkillProjectDir: ".claude/skills"},
	"opencode": {Name: "opencode",
		Add:        []string{"opencode", "mcp", "add", "--global", "factory", "--", "{CMD}", "mcp"},
		AddProject: []string{"opencode", "mcp", "add", "factory", "--", "{CMD}", "mcp"},
		Snippet:    opencodeJSON, SnippetProject: opencodeJSON,
		SnippetPath: "~/.config/opencode/opencode.json", SnippetProjectPath: "opencode.json",
		SkillDir: "~/.agents/skills", SkillProjectDir: ".agents/skills"},
	"codex": {Name: "codex",
		Add:     []string{"codex", "mcp", "add", "factory", "--", "{CMD}", "mcp"},
		Snippet: codexTOML, SnippetProject: codexTOML,
		SnippetPath: "~/.codex/config.toml", SnippetProjectPath: ".codex/config.toml",
		SkillDir: "~/.agents/skills", SkillProjectDir: ".agents/skills"},
	"gemini": {Name: "gemini",
		Add:        []string{"gemini", "mcp", "add", "--scope", "user", "factory", "{CMD}", "mcp"},
		AddProject: []string{"gemini", "mcp", "add", "--scope", "project", "factory", "{CMD}", "mcp"},
		Snippet:    mcpServersJSON, SnippetProject: mcpServersJSON,
		SnippetPath: "~/.gemini/settings.json", SnippetProjectPath: ".gemini/settings.json",
		SkillDir: "~/.gemini/skills", SkillProjectDir: ".gemini/skills",
		Note: "gemini connects a stdio server only in a trusted folder: accept its trust dialog the first time you run gemini there (saved in ~/.gemini/trustedFolders.json)."},
	"cursor": {Name: "cursor",
		Snippet: cursorJSON, SnippetProject: cursorJSON,
		SnippetPath: "~/.cursor/mcp.json", SnippetProjectPath: ".cursor/mcp.json",
		SkillDir: "~/.cursor/skills", SkillProjectDir: ".cursor/skills"},
	"copilot": {Name: "copilot",
		Snippet: copilotJSON, SnippetProject: copilotJSON,
		SnippetPath: "~/.copilot/mcp-config.json", SnippetProjectPath: ".github/mcp.json",
		SkillDir: "~/.copilot/skills", SkillProjectDir: ".github/skills"},
}

// LookupAgent finds the agent a setup argument names, or names the six.
func LookupAgent(name string) (Agent, error) {
	if a, ok := Agents[name]; ok {
		return a, nil
	}
	return Agent{}, fmt.Errorf("unknown client %q: one of %s", name, strings.Join(slices.Sorted(maps.Keys(Agents)), ", "))
}

func (a Agent) skillDir(project bool) string {
	if project {
		return a.SkillProjectDir
	}
	return a.SkillDir
}

// SkillDirs is every directory an agent reads skills from, each once: every agent's SkillDir under
// home and SkillProjectDir under dir.
func SkillDirs(home, dir string) []string {
	var out []string
	for _, k := range slices.Sorted(maps.Keys(Agents)) {
		for _, p := range []string{Agents[k].SkillDir, Agents[k].SkillProjectDir} {
			s := Setup{Home: home, Dir: dir}.resolve(p)
			if !slices.Contains(out, s) {
				out = append(out, s)
			}
		}
	}
	return out
}

// SetupCmd is the command the agent will run: roomctl when the PATH finds it, else the absolute path
// of this executable, so a binary that is not installed still works.
func SetupCmd(lookPath func(string) (string, error), executable func() (string, error)) string {
	if _, err := lookPath("roomctl"); err == nil {
		return "roomctl"
	}
	if p, err := executable(); err == nil {
		return p
	}
	return "roomctl"
}

// Setup is one `roomctl setup <agent>`: it registers `roomctl mcp` through the agent's own CLI when
// that exists and never edits a config file itself, else it prints the snippet. It installs the
// skill, the one thing it writes. Print writes and runs nothing.
type Setup struct {
	Agent          Agent
	Project, Print bool
	Home, Dir      string // the home directory, and the repository
	Cmd, Version   string
	LookPath       func(string) (string, error)
	Run            func(ctx context.Context, argv []string) error
	Out            io.Writer
}

// resolve places a ~/ path under home and any other under dir.
func (s Setup) resolve(p string) string {
	if rest, ok := strings.CutPrefix(p, "~/"); ok {
		return filepath.Join(s.Home, rest)
	}
	return filepath.Join(s.Dir, p)
}

// quoted escapes cmd for the inside of a JSON or TOML basic string.
func quoted(cmd string) string {
	q := strconv.Quote(cmd)
	return q[1 : len(q)-1]
}

// Do runs the setup.
func (s Setup) Do(ctx context.Context) error {
	a := s.Agent
	argv, snippet, path := a.Add, a.Snippet, a.SnippetPath
	if s.Project {
		argv, snippet, path = a.AddProject, a.SnippetProject, a.SnippetProjectPath
	}
	snippet = strings.ReplaceAll(snippet, "{CMD}", quoted(s.Cmd))
	argv = slices.Clone(argv)
	for i, v := range argv {
		if v == "{CMD}" {
			argv[i] = s.Cmd
		}
	}
	skillDir := a.skillDir(s.Project)
	warn := s.warning(path)
	if s.Print {
		return s.print(argv, snippet, path, skillDir, warn)
	}
	if fi, err := os.Lstat(s.resolve(skillDir)); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink: refusing to install the skill through it", s.resolve(skillDir))
	}
	// The skill first: a failed install leaves nothing half set up, and a second run refreshes it.
	paths, err := skill.Install(s.resolve(skillDir), s.Version)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.Out, "installed the factory-handoff skill in %s (%d files)\n", s.resolve(skillDir), len(paths)); err != nil {
		return err
	}
	if len(argv) > 0 {
		if _, err := s.LookPath(argv[0]); err != nil {
			if _, err := fmt.Fprintf(s.Out, "%s is not on the PATH, so nothing registered the server.\n", argv[0]); err != nil {
				return err
			}
			argv = nil
		}
	}
	if len(argv) > 0 {
		line := "registered the factory MCP server: " + strings.Join(argv, " ")
		if err := s.Run(ctx, argv); err != nil {
			if !alreadyRegistered(err) {
				return fmt.Errorf("%s: %w", strings.Join(argv, " "), err)
			}
			line = "the factory MCP server is already registered: " + strings.Join(argv, " ") + " refused a duplicate, which is fine"
		}
		if _, err := fmt.Fprintln(s.Out, line); err != nil {
			return err
		}
	} else if err := s.printSnippet(snippet, path); err != nil {
		return err
	}
	_, err = io.WriteString(s.Out, s.notes(warn))
	return err
}

// alreadyRegistered reads the CLI's refusal of a duplicate name. The CLIs have no scope-exact
// "is it there" query (claude mcp get ignores the scope), so the add's own answer decides.
func alreadyRegistered(err error) bool {
	m := strings.ToLower(err.Error())
	return strings.Contains(m, "already exists") || strings.Contains(m, "already configured")
}

// warning is set when the snippet to commit names this machine's executable.
func (s Setup) warning(path string) string {
	if !s.Project || !filepath.IsAbs(s.Cmd) {
		return ""
	}
	return fmt.Sprintf("warning: %s names %s, which your teammates' checkouts will not have: put roomctl on the PATH and run setup again, or keep this file out of git.\n", path, s.Cmd)
}

// notes is what follows the setup: the warning, then the agent's own note.
func (s Setup) notes(warn string) string {
	if s.Agent.Note == "" {
		return warn
	}
	return warn + s.Agent.Note + "\n"
}

func (s Setup) printSnippet(snippet, path string) error {
	_, err := fmt.Fprintf(s.Out, "add this to %s (roomctl setup never edits a config):\n\n%s", path, snippet)
	return err
}

func (s Setup) print(argv []string, snippet, path, skillDir, warn string) error {
	var errs []error
	if len(argv) > 0 {
		_, err := fmt.Fprintf(s.Out, "with %s on the PATH, setup runs:\n  %s\n\notherwise: ", argv[0], strings.Join(argv, " "))
		errs = append(errs, err)
	}
	errs = append(errs, s.printSnippet(snippet, path))
	_, err := fmt.Fprintf(s.Out, "\nsetup installs the factory-handoff skill in %s\n%s", skillDir, s.notes(warn))
	return errors.Join(append(errs, err)...)
}
