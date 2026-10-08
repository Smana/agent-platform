// SPDX-License-Identifier: Apache-2.0

// Package skill ships the factory-handoff Agent Skill inside the roomctl binary, so the skill a
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
)

// all: keeps references/ and any dotfile the skill grows.
//
//go:embed all:factory-handoff
var files embed.FS

const name = "factory-handoff"

// Install writes the skill under dir/factory-handoff, stamping version into SKILL.md, and
// returns the paths written. It overwrites existing files, and refuses a symlink anywhere in the
// skill tree: dir is user input and a write must never leave it.
func Install(dir, version string) ([]string, error) {
	root := filepath.Join(dir, name)
	if fi, err := os.Lstat(root); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("%s is a symlink: refusing to write through it", root)
	} else if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
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
