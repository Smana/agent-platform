// SPDX-License-Identifier: Apache-2.0

package roomctl

import (
	"bytes"
	"context"
	"encoding/json"
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
	// CommandDir and CommandProjectDir hold the agent's custom slash commands, where setup writes
	// /factory; empty for an agent with none.
	CommandDir, CommandProjectDir string
	Note                          string // printed after setup: what the agent still needs
}

const (
	mcpServersJSON = "{\n  \"mcpServers\": {\n    \"factory\": {\n      \"command\": \"{CMD}\",\n      \"args\": [\"mcp\"]\n    }\n  }\n}\n"
	copilotJSON    = "{\n  \"mcpServers\": {\n    \"factory\": {\n      \"type\": \"local\",\n      \"command\": \"{CMD}\",\n      \"args\": [\"mcp\"],\n      \"tools\": [\"*\"]\n    }\n  }\n}\n"
	cursorJSON     = "{\n  \"mcpServers\": {\n    \"factory\": {\n      \"type\": \"stdio\",\n      \"command\": \"{CMD}\",\n      \"args\": [\"mcp\"]\n    }\n  }\n}\n"
	// opencodeJSON is what opencode 2.0.22's own `mcp add` writes to ./opencode.json (observed):
	// mcp.servers.<name>, not the mcp.<name> its docs show. The CLI is the authority for the version
	// we target.
	// timeout.execution (ms) outlives issue_file's form: OpenCode v2 otherwise cuts the tool (O10).
	// "codemode": false keeps the server's tools on the native list: OpenCode v2 otherwise exposes
	// them through Code Mode, which an `execute` deny rule turns off, hiding the server.
	opencodeJSON = "{\n  \"mcp\": {\n    \"servers\": {\n      \"factory\": {\n        \"type\": \"local\",\n        \"command\": [\"{CMD}\", \"mcp\"],\n        \"codemode\": false,\n        \"timeout\": {\"execution\": 900000}\n      }\n    }\n  }\n}\n"
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
		SkillDir: "~/.claude/skills", SkillProjectDir: ".claude/skills",
		CommandDir: "~/.claude/commands", CommandProjectDir: ".claude/commands"},
	"opencode": {Name: "opencode",
		Add:        []string{"opencode", "mcp", "add", "--global", "factory", "--", "{CMD}", "mcp"},
		AddProject: []string{"opencode", "mcp", "add", "factory", "--", "{CMD}", "mcp"},
		Snippet:    opencodeJSON, SnippetProject: opencodeJSON,
		SnippetPath: "~/.config/opencode/opencode.json", SnippetProjectPath: "opencode.json",
		SkillDir: "~/.agents/skills", SkillProjectDir: ".agents/skills",
		CommandDir: "~/.config/opencode/commands", CommandProjectDir: ".opencode/commands"},
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

func (a Agent) commandDir(project bool) string {
	if project {
		return a.CommandProjectDir
	}
	return a.CommandDir
}

// SkillDirs is every directory an agent reads skills from, each once: every agent's SkillDir under
// home and SkillProjectDir under dir; an empty dir (no repository) has no project scope.
func SkillDirs(home, dir string) []string {
	var out []string
	for _, k := range slices.Sorted(maps.Keys(Agents)) {
		for _, p := range []string{Agents[k].SkillDir, Agents[k].SkillProjectDir} {
			if dir == "" && !strings.HasPrefix(p, "~/") {
				continue
			}
			s := Setup{Home: home, Dir: dir}.resolve(p)
			if !slices.Contains(out, s) {
				out = append(out, s)
			}
		}
	}
	return out
}

// OpenCodeConfigs are the user-level OpenCode config files setup writes and the doctor reads.
func OpenCodeConfigs(home string) []string {
	return []string{filepath.Join(home, ".config", "opencode", "opencode.json"), filepath.Join(home, ".config", "opencode", "opencode.jsonc")}
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
// that exists and edits no config file itself but the codemode flag of the OpenCode entry it just registered, else it prints the snippet. It installs the
// skill, the one thing it writes. Print writes and runs nothing.
type Setup struct {
	Agent          Agent
	Project, Print bool
	Home, Dir      string // the home directory, and the repository
	Cmd, Version   string
	LookPath       func(string) (string, error)
	Run            func(ctx context.Context, argv []string) error
	Out            io.Writer
	// Capture runs argv and returns its output; nil skips the OpenCode service check.
	Capture func(ctx context.Context, argv []string) (string, error)
	// Ask puts a yes/no question to the developer; nil when there is no terminal.
	Ask func(prompt string) (bool, error)
	// RestartService restarts OpenCode's background service without asking (--restart-service).
	RestartService bool
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
	if cd := a.commandDir(s.Project); cd != "" {
		p, err := skill.InstallCommand(s.resolve(cd))
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(s.Out, "installed the /factory command in %s: /factory handoff, /factory status, /factory inbox\n", p); err != nil {
			return err
		}
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
		if err := s.opencodeCodeMode(snippetPath(s, path)); err != nil {
			return err
		}
		if err := s.opencodeService(ctx); err != nil {
			return err
		}
	} else if err := s.printSnippet(snippet, path); err != nil {
		return err
	}
	_, err = io.WriteString(s.Out, s.notes(warn))
	return err
}

// snippetPath is the resolved config file a setup edits or tells the developer about.
func snippetPath(s Setup, path string) string { return s.resolve(path) }

// opencodeExecutionTimeout is the tool execution timeout (ms) setup writes: the form issue_file shows
// waits for a human, and OpenCode cut it with "context deadline exceeded".
const opencodeExecutionTimeout = 900000

// executionTimeout is mcp.servers.<factory>.timeout.execution in ms, 0 when absent.
func executionTimeout(factory map[string]any) float64 {
	t, _ := factory["timeout"].(map[string]any)
	switch v := t["execution"].(type) {
	case json.Number:
		f, _ := v.Float64()
		return f
	case float64:
		return v
	case int:
		return float64(v)
	}
	return 0
}

// opencodeCodeMode sets "codemode": false on mcp.servers.factory of the config `opencode mcp add`
// just wrote, keeping the rest of the file. It is the one config edit setup makes, and it refuses a
// file that is not plain JSON (comments), telling the developer what to add instead.
func (s Setup) opencodeCodeMode(path string) error {
	if s.Agent.Name != "opencode" {
		return nil
	}
	tell := func(why string) error {
		_, err := fmt.Fprintf(s.Out, "%s: %s: add \"codemode\": false and \"timeout\": {\"execution\": 900000} to mcp.servers.factory yourself, or OpenCode's Code Mode (which an execute deny rule turns off) may hide the factory tools, and its tool timeout may cut issue_file's form\n", path, why)
		return err
	}
	b, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return tell("not found")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var cfg map[string]any
	if err := dec.Decode(&cfg); err != nil {
		return tell("not plain JSON, left untouched")
	}
	servers := mapAt(mapAt(cfg, "mcp"), "servers")
	factory, ok := servers["factory"].(map[string]any)
	if !ok {
		return tell("no mcp.servers.factory")
	}
	changed := false
	if factory["codemode"] != false {
		factory["codemode"], changed = false, true
	}
	if executionTimeout(factory) < opencodeExecutionTimeout {
		t := map[string]any{}
		switch old := factory["timeout"].(type) {
		case map[string]any:
			t = old
		case json.Number: // a v1-style number: the catalog's
			t["catalog"] = old
		}
		t["execution"], changed = opencodeExecutionTimeout, true
		factory["timeout"] = t
	}
	if !changed {
		return nil
	}
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(out, '\n'), 0o644); err != nil { //nolint:gosec // a config the developer owns
		return err
	}
	_, err = fmt.Fprintf(s.Out, "set \"codemode\": false and timeout.execution %d on mcp.servers.factory in %s: its tools stay on OpenCode's native list, and issue_file's form can wait for you\n", opencodeExecutionTimeout, path)
	return err
}

// mapAt is m[key] as an object, nil when it is not one.
func mapAt(m map[string]any, key string) map[string]any {
	v, _ := m[key].(map[string]any)
	return v
}

// opencodeService tells a developer whose OpenCode background service is running that it keeps the
// config it started with: sessions attached to it have no factory tools, though `opencode mcp list`
// says connected. It restarts the service only on a yes, never silently, since other sessions may be
// attached. Without the binary's service subcommand it prints the hint alone.
func (s Setup) opencodeService(ctx context.Context) error {
	if s.Agent.Name != "opencode" || s.Capture == nil {
		return nil
	}
	const restart = "opencode service restart"
	out, err := s.Capture(ctx, []string{"opencode", "service", "status"})
	if err != nil {
		_, err = fmt.Fprintf(s.Out, "could not check %q: if you use OpenCode's background service, run `%s` so its sessions see the factory tools\n", "opencode service status", restart)
		return err
	}
	if !strings.Contains(out, "://") { // status prints the service's URL while it runs
		return nil
	}
	if _, err := fmt.Fprintf(s.Out, "OpenCode's background service is running (%s) and keeps the config it started with: sessions attached to it have no factory tools until it restarts (`%s`, which also disconnects them).\n", OneLine(out), restart); err != nil {
		return err
	}
	yes := s.RestartService
	if !yes && s.Ask != nil {
		var aerr error
		if yes, aerr = s.Ask("Restart it now? Other OpenCode sessions attached to it will be disconnected. [y/N] "); aerr != nil {
			return aerr
		}
	}
	if !yes {
		_, err := fmt.Fprintln(s.Out, "not restarted: run it yourself when no session needs the service")
		return err
	}
	if err := s.Run(ctx, []string{"opencode", "service", "restart"}); err != nil {
		return fmt.Errorf("opencode service restart: %w", err)
	}
	_, err = fmt.Fprintln(s.Out, "restarted OpenCode's background service")
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
	extra := ""
	if cd := s.Agent.commandDir(s.Project); cd != "" {
		extra = fmt.Sprintf("and the /factory command (handoff, status, inbox) in %s\n", cd)
	}
	_, err := fmt.Fprintf(s.Out, "\nsetup installs the factory-handoff skill in %s\n%s%s", skillDir, extra, s.notes(warn))
	return errors.Join(append(errs, err)...)
}
