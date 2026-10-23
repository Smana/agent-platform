// SPDX-License-Identifier: Apache-2.0

package steering

import (
	"strings"
	"testing"
)

// The scoper design's docs note.
const docs = "# Steering: docs\n\n## Verify\n- `task links` (CI: validate): every relative Markdown link resolves.\n" +
	"- `task docs` (CI: validate): the site builds; every repository path it names exists.\n\n## Leave alone\n" +
	"- `docs/superpowers/`: dated designs and plans; edit only the one the issue names.\n" +
	"- `website/content/docs/decisions/`: an ADR is superseded by a new one, never edited.\n\n## Write\n" +
	"- English, short declarative sentences, tables over prose; a host page by absolute URL.\n"

func TestTheDesignsNoteIsValid(t *testing.T) {
	if err := Validate(docs); err != nil {
		t.Fatal(err)
	}
	if err := Validate("## Verify\n- `go test ./...` (CI: check)\n"); err != nil {
		t.Fatalf("no title, one module: %v", err)
	}
}

// SC4, offline proof 2: 2049 bytes, four modules or no ## Verify is not used.
func TestANoteOutsideSC4IsRefused(t *testing.T) {
	over := docs[:len(docs)-1] + strings.Repeat("x", MaxBytes-len(docs)+1) + "\n"
	if len(over) != MaxBytes+1 {
		t.Fatalf("the 2049-byte case is %d bytes", len(over))
	}
	for name, c := range map[string]struct{ text, want string }{
		"2049 bytes":           {over, "at most 2048"},
		"four modules":         {docs + "## More\n- x\n", "4 modules"},
		"no Verify":            {"# Steering: docs\n\n## Leave alone\n- `docs/`\n", "no ## Verify"},
		"an empty Verify":      {"## Verify\nRun the tests.\n## Leave alone\n- x\n", "lists no check"},
		"a Verify look-alike":  {"## Verify the site\n- x\n", "no ## Verify"},
		"text before a module": {"Read this first.\n## Verify\n- x\n", "before the first module"},
		"a title on line 2":    {"\n# Steering\n## Verify\n- x\n", "before the first module"},
		"not UTF-8":            {"## Verify\n- \xff\n", "not UTF-8"},
		"a module in a fence":  {"## Verify\n- x\n```\n## A\n## B\n## C\n```\n", "4 modules"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := Validate(c.text); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
}

// The class names a file: anything but lowercase letters, digits and hyphens reads none.
func TestPathRefusesAClassThatIsNotAName(t *testing.T) {
	if got, want := Path("docs-links"), ".github/agent-factory/steering/docs-links.md"; got != want {
		t.Fatalf("Path = %q, want %q", got, want)
	}
	for _, class := range []string{"", "..", "../x", "a/b", "Docs", "docs.md", "a b", "docs\n", "d\u00f6cs"} {
		if got := Path(class); got != "" {
			t.Errorf("Path(%q) = %q, want none", class, got)
		}
	}
}

// A note reaches an agent's brief: no control characters but newline and tab, no Unicode format
// runes (zero-width, bidi), and no carriage return, which CRLF input would bring and which can
// hide text on a terminal. Refused, not normalized: the bytes the sha names are the bytes used.
func TestAnInvisibleCharacterIsRefused(t *testing.T) {
	for name, text := range map[string]string{
		"a NUL":           "## Verify\n- x\x00\n",
		"an escape":       "## Verify\n- \x1b[2Jx\n",
		"a CR":            "## Verify\n- x\r\n",
		"a bare CR":       "## Verify\n- x\r- y\n",
		"a zero-width":    "## Verify\n- a\u200bb\n",
		"a bidi override": "## Verify\n- a\u202eb\n",
		"a BOM":           "\ufeff## Verify\n- x\n",
	} {
		t.Run(name, func(t *testing.T) {
			if err := Validate(text); err == nil || !strings.Contains(err.Error(), "character") {
				t.Fatalf("err = %v", err)
			}
		})
	}
	if err := Validate("## Verify\n- a\tb\n"); err != nil {
		t.Fatalf("a tab: %v", err)
	}
}
