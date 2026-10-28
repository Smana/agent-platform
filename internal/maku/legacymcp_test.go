// SPDX-License-Identifier: Apache-2.0

package maku

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// An agent set up with roomctl keeps a factory entry that runs `roomctl mcp`: the add refuses the
// duplicate, and setup must say the entry is stale and how to replace it, never "which is fine".
func TestSetupFlagsAFactoryEntryThatStillRunsRoomctl(t *testing.T) {
	for _, tc := range []struct {
		name, agent string
		project     bool
		file, body  string
		fix         string // "" when the entry is maku's and the duplicate is fine
	}{
		{"claude, user scope", "claude", false, ".claude.json", `{"mcpServers":{"factory":{"command":"roomctl","args":["mcp"]}}}`,
			"claude mcp remove --scope user factory"},
		{"claude, an absolute roomctl", "claude", false, ".claude.json", `{"mcpServers":{"factory":{"command":"/usr/local/bin/roomctl","args":["mcp"]}}}`,
			"claude mcp remove --scope user factory"},
		{"gemini, project scope", "gemini", true, ".gemini/settings.json", `{"mcpServers":{"factory":{"command":"roomctl","args":["mcp"]}}}`,
			"gemini mcp remove --scope project factory"},
		{"codex", "codex", false, ".codex/config.toml", "[other]\ncommand = \"x\"\n\n[mcp_servers.factory]\ncommand = \"roomctl\"\nargs = [\"mcp\"]\n",
			"codex mcp remove factory"},
		{"opencode, a command array", "opencode", false, ".config/opencode/opencode.json", `{"mcp":{"servers":{"factory":{"type":"local","command":["roomctl","mcp"]}}}}`,
			"delete the factory entry from"},
		{"claude, already maku", "claude", false, ".claude.json", `{"mcpServers":{"factory":{"command":"maku","args":["mcp"]}}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, dir := t.TempDir(), t.TempDir()
			root := home
			if tc.project {
				root = dir
			}
			p := filepath.Join(root, tc.file)
			if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			s := Setup{Agent: Agents[tc.agent], Project: tc.project, Home: home, Dir: dir, Cmd: "maku", Version: "v1", LookPath: found, Out: &out,
				Run: func(_ context.Context, argv []string) error {
					if argv[1] == "mcp" && argv[2] == "add" {
						return errors.New("exit status 1: MCP server factory already exists")
					}
					return nil
				},
				Capture: func(context.Context, []string) (string, error) { return "", errors.New("no service") }}
			if err := s.Do(t.Context()); err != nil {
				t.Fatal(err)
			}
			got := out.String()
			if tc.fix == "" {
				if !strings.Contains(got, "which is fine") || strings.Contains(got, "roomctl") {
					t.Fatalf("a maku entry is fine:\n%s", got)
				}
				return
			}
			setup := "maku setup " + tc.agent
			if tc.project {
				setup += " --project"
			}
			if strings.Contains(got, "which is fine") || !strings.Contains(got, "still runs roomctl") ||
				!strings.Contains(got, tc.fix) || !strings.Contains(got, setup) || !strings.Contains(got, p) {
				t.Fatalf("want the stale entry (%s), %q and %q named:\n%s", p, tc.fix, setup, got)
			}
		})
	}
}

// The doctor warns about every factory entry, in any agent's config, that still runs roomctl.
func TestDoctorWarnsOnAFactoryEntryThatStillRunsRoomctl(t *testing.T) {
	for _, tc := range []struct {
		name, cmd string
		warn      bool
	}{{"roomctl", "roomctl", true}, {"maku", "maku", false}} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			p := filepath.Join(home, ".claude.json")
			if err := os.WriteFile(p, []byte(`{"mcpServers":{"factory":{"command":"`+tc.cmd+`","args":["mcp"]}}}`), 0o600); err != nil {
				t.Fatal(err)
			}
			r := newDoctorRig(t)
			r.d.LegacyMCP = LegacyEntries(home, "")
			var out strings.Builder
			if err := r.d.Run(t.Context(), &out); err != nil {
				t.Fatalf("%v\n%s", err, &out)
			}
			got := strings.Contains(out.String(), "warn mcp: "+p+": the factory MCP server still runs roomctl")
			if got != tc.warn || (tc.warn && !strings.Contains(out.String(), "claude mcp remove --scope user factory")) {
				t.Fatalf("warn=%v want %v\n%s", got, tc.warn, &out)
			}
		})
	}
}
