// SPDX-License-Identifier: Apache-2.0

package maku

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// legacyName is the directory roomctl, maku's name before v0.30.0, kept its files in.
const legacyName = "roomctl"

// ImportLegacy copies roomctl's files (config.json, token.json, cursors.json) from
// <base>/roomctl into <base>/maku, once: only while <base>/maku does not exist, so a later run,
// or a developer who configured maku first, never reads the old directory again. The copy is
// built in a temporary directory and renamed into place, so a failure leaves no half import. The
// old directory stays, and roomctl keeps working for a rollback. It reports whether it imported.
func ImportLegacy(base string) (bool, error) {
	dst, src := filepath.Join(base, "maku"), filepath.Join(base, legacyName)
	if _, err := os.Stat(dst); !errors.Is(err, fs.ErrNotExist) {
		return false, err // nil when dst exists: nothing to do
	}
	entries, err := os.ReadDir(src)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("import %s: %w", src, err)
	}
	tmp, err := os.MkdirTemp(base, ".maku-import-") // 0700
	if err != nil {
		return false, fmt.Errorf("import %s: %w", src, err)
	}
	defer func() { _ = os.RemoveAll(tmp) }() // a no-op once renamed
	for _, e := range entries {
		if !e.Type().IsRegular() || strings.HasPrefix(e.Name(), ".") {
			continue // a directory, a link, or a temp file SaveJSON left behind
		}
		b, err := os.ReadFile(filepath.Join(src, e.Name())) //nolint:gosec // the user's own config directory
		if err != nil {
			return false, fmt.Errorf("import %s: %w", e.Name(), err)
		}
		if err := writeFile(filepath.Join(tmp, e.Name()), b); err != nil {
			return false, fmt.Errorf("import %s: %w", e.Name(), err)
		}
	}
	if err := os.Rename(tmp, dst); err != nil {
		return false, fmt.Errorf("import into %s: %w", dst, err)
	}
	return true, nil
}
