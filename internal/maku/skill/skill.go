// SPDX-License-Identifier: Apache-2.0

// Package skill ships the factory-handoff Agent Skill inside the maku binary, so the skill a
// developer installs always matches the CLI that will run its commands.
package skill

import (
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"sigs.k8s.io/yaml"
)

// all: keeps references/ and any dotfile the skill grows.
//
//go:embed all:factory-handoff
var files embed.FS

const name = "factory-handoff"

// command is the /factory slash command, one markdown file that Claude Code and OpenCode both
// read from their commands directory: the short spelling of the MCP prompts, which autocomplete as
// /factory:handoff but are only accepted typed as /mcp__factory__handoff.
//
//go:embed commands/factory.md
var command []byte

// CommandFile is the command's file name.
const CommandFile = "factory.md"

// InstallCommand writes the /factory command into dir and returns its path. It refuses a symlink
// at the file, as Install does.
func InstallCommand(dir string) (string, error) {
	dst := filepath.Join(dir, CommandFile)
	if fi, err := os.Lstat(dst); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%s is a symlink: refusing to write through it", dst)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // a repo file, not a secret
		return "", err
	}
	return dst, os.WriteFile(dst, command, 0o644) //nolint:gosec // same
}

// Install writes the skill under dir/factory-handoff, stamping version into SKILL.md, and
// returns the paths written. It overwrites existing files, and refuses a symlink anywhere in the
// skill tree: dir is user input and a write must never leave it.
func Install(dir, version string) ([]string, error) {
	var written []string
	err := fs.WalkDir(files, name, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		dst := filepath.Join(dir, filepath.FromSlash(p))
		if fi, err := os.Lstat(dst); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s is a symlink: refusing to write through it", dst)
		}
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755) //nolint:gosec // committed to the repo and read by other users: not a secret
		}
		b, err := files.ReadFile(p)
		if err != nil {
			return err
		}
		if p == name+"/SKILL.md" {
			b = []byte(strings.ReplaceAll(string(b), "{{VERSION}}", version))
		}
		if err := os.WriteFile(dst, b, 0o644); err != nil { //nolint:gosec // same: a repo file, not a secret
			return err
		}
		written = append(written, dst)
		return nil
	})
	return written, err
}

// Version is the metadata.maku-version of dir/factory-handoff/SKILL.md, or its
// metadata.roomctl-version when roomctl installed it. It reports false when the file is absent,
// and an error when the front matter is not YAML.
func Version(dir string) (string, bool, error) {
	p := filepath.Join(dir, name, "SKILL.md")
	b, err := os.ReadFile(p) //nolint:gosec // dir is an agent skill directory resolved by setup, not request input
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	parts := strings.SplitN(strings.ReplaceAll(string(b), "\r\n", "\n"), "---\n", 3)
	if len(parts) != 3 || parts[0] != "" {
		return "", false, fmt.Errorf("%s has no front matter", p)
	}
	var fm struct {
		Metadata map[string]any `json:"metadata"`
	}
	if err := yaml.Unmarshal([]byte(parts[1]), &fm); err != nil {
		return "", false, fmt.Errorf("%s: front matter is not YAML: %w", p, err)
	}
	v, _ := fm.Metadata["maku-version"].(string)
	if v == "" {
		v, _ = fm.Metadata["roomctl-version"].(string) // installed by roomctl, before v0.30.0
	}
	if v == "" {
		return "", false, fmt.Errorf("%s has no metadata.maku-version: run maku setup <client> to rewrite it", p)
	}
	return v, true, nil
}
