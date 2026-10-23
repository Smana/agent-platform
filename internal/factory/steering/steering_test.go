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
