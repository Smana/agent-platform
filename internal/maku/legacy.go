// SPDX-License-Identifier: Apache-2.0

package maku

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// legacyName is the directory roomctl, maku's name before v0.30.0, kept its files in.
const legacyName = "roomctl"

// ImportLegacy copies roomctl's files (config.json, token.json, cursors.json) from
// <base>/roomctl into <base>/maku, once: only while <base>/maku does not exist, so a later run,
// or a developer who configured maku first, never reads the old directory again. The copy is
// built in a temporary directory and renamed into place, so a failure leaves no half import. The
// old directory stays, and roomctl keeps working for a rollback. A symlink is not followed: note
// gets one line naming it, so the developer knows why they log in again; nil note is silent. A
// concurrent first run that renames first wins, and this one reports that it imported nothing. It
// reports whether it imported.
func ImportLegacy(base string, note io.Writer) (bool, error) {
	return importLegacy(base, note, nil)
}

// importLegacy is ImportLegacy with a hook run just before the rename, where a test plays the
// concurrent first run.
func importLegacy(base string, note io.Writer, beforeRename func()) (bool, error) {
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
		if e.Type()&fs.ModeSymlink != 0 && note != nil && !strings.HasPrefix(e.Name(), ".") {
			_, _ = fmt.Fprintf(note, "maku: %s is a symlink, not imported from roomctl's config: run maku configure or maku login again if maku asks\n",
				filepath.Join(src, e.Name()))
		}
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
	if beforeRename != nil {
		beforeRename()
	}
	if err := os.Rename(tmp, dst); err != nil {
		if errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, fs.ErrExist) {
			return false, nil // a concurrent first run imported, or maku made its own directory, first
		}
		return false, fmt.Errorf("import into %s: %w", dst, err)
	}
	return true, nil
}
