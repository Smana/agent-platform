// SPDX-License-Identifier: Apache-2.0

// Package steering is a task class's steering note (scoper design SC3, SC4): a short Markdown file
// a repository's maintainers keep at .github/agent-factory/steering/<class>.md, read on the default
// branch at triage. `.github/` is a gate path, so no agent pull request can change one.
package steering

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxBytes and MaxModules are SC4's bounds: two or three focused modules, never a manual.
const (
	MaxBytes   = 2048
	MaxModules = 3
	verify     = "## Verify"
)

var className = regexp.MustCompile(`^[a-z0-9-]+$`)

// Path is class's note in the repository it describes, "" for a class that is not a name
// ([a-z0-9-]+): a "/" or ".." must never reach another file. Callers read "" as no note.
func Path(class string) string {
	if !className.MatchString(class) {
		return ""
	}
	return ".github/agent-factory/steering/" + class + ".md"
}

// Validate is SC4's format, the same in ai-native-ref's suite: UTF-8, at most 2048 bytes, no
// control (but newline and tab) or Unicode format character, an optional "# " title on line 1, at
// most three "## " modules, one of them "## Verify" with at least one "- " or "* " item. Before the first module only the title and blank lines may stand.
// A "## " line counts wherever it stands, inside a code fence too: a fence never hides a module.
func Validate(text string) error {
	if !utf8.ValidString(text) {
		return errors.New("steering: not UTF-8")
	}
	if len(text) > MaxBytes {
		return fmt.Errorf("steering: %d bytes, at most %d", len(text), MaxBytes)
	}
	// The note lands in an agent's brief: refuse what hides text. A CR is refused, not normalized,
	// so the bytes the blob sha names are the bytes used.
	for _, c := range text {
		if (unicode.IsControl(c) && c != '\n' && c != '\t') || unicode.Is(unicode.Cf, c) {
			return fmt.Errorf("steering: control or invisible character U+%04X", c)
		}
	}
	modules, items, in, hasVerify := 0, 0, "", false
	for i, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "## "):
			modules++
			in = strings.TrimRight(line, " \t")
			hasVerify = hasVerify || in == verify
		case strings.HasPrefix(line, "# ") && i == 0:
		case in == "" && strings.TrimSpace(line) != "":
			return fmt.Errorf("steering: line %d stands before the first module", i+1)
		case in == verify && (strings.HasPrefix(line, "- ") || strings.HasPrefix(line, "* ")):
			items++
		}
	}
	switch {
	case modules > MaxModules:
		return fmt.Errorf("steering: %d modules, at most %d", modules, MaxModules)
	case !hasVerify:
		return errors.New("steering: no ## Verify module")
	case items == 0:
		return errors.New("steering: ## Verify lists no check")
	}
	return nil
}
