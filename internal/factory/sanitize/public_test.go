// SPDX-License-Identifier: Apache-2.0

package sanitize

import (
	"strings"
	"testing"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	gmtext "github.com/yuin/goldmark/text"
)

// links are the nodes GitHub would render as something a reader can click or that loads.
func links(out string) []string {
	var found []string
	doc := goldmark.New(goldmark.WithExtensions(extension.GFM)).Parser().Parse(gmtext.NewReader([]byte(out)))
	_ = ast.Walk(doc, func(n ast.Node, _ bool) (ast.WalkStatus, error) {
		switch n.Kind() {
		case ast.KindLink, ast.KindAutoLink, ast.KindRawHTML, ast.KindHTMLBlock, ast.KindImage:
			found = append(found, n.Kind().String())
		}
		return ast.WalkContinue, nil
	})
	return found
}

// R8, offline proof 3: a hostile proposal comes out with no link, no raw HTML and no hidden character.
func TestPublicIsInert(t *testing.T) {
	in := "## What is wrong\nsee [the fix](https://evil.example/x) or https://evil.example/y\n" +
		"<img src=x onerror=alert(1)> <b>bold</b>\nbidi \u202eevil\u202c here"
	out, rep := Public(in)
	if strings.ContainsRune(out, '\u202e') || rep.Invisible == 0 {
		t.Fatalf("bidi left: %q", out)
	}
	if l := links(out); len(l) != 0 {
		t.Errorf("%v survived in %q", l, out)
	}
	// A mention right after an unpaired backtick or an underscore is live on GitHub too.
	if m, _ := Public("a `@z b _@n_ c"); !strings.Contains(m, "`@z`") || !strings.Contains(m, "`@n`") {
		t.Errorf("a mention after a backtick or an underscore: %q", m)
	}
	if !strings.Contains(out, "`https://evil.example/x`") || !strings.Contains(out, "`https://evil.example/y`") {
		t.Fatalf("each URL is kept, inert, in a code span: %q", out)
	}
}

// The attacks on the code-span rule itself: a backtick that pairs with the one Public adds, a
// destination the link pattern does not take, an autolink with no scheme, an email, an escape.
func TestPublicSurvivesBacktickTricks(t *testing.T) {
	for name, in := range map[string]string{
		"a backtick in the destination": "[a](https://evil.example/x`y)",
		"an unmatched backtick before":  "a ` b https://evil.example/z",
		"an unmatched run before":       "a `` b https://evil.example/z `",
		"an escaped backtick":           "a \\` b https://evil.example/z \\`",
		"a title":                       `[a](https://evil.example/x "t")`,
		"a nested bracket":              "[a [b]](https://evil.example/x)",
		"a www host":                    "see www.evil.example/x",
		"an email":                      "write to someone@evil.example",
		"ftp":                           "ftp://evil.example/x",
		"an angle autolink":             "<https://evil.example/x>",
		"a reference link":              "[a][r]\n\n[r]: https://evil.example/x",
		"inside an existing span":       "`https://evil.example/in-span` and https://evil.example/out",
	} {
		t.Run(name, func(t *testing.T) {
			out, _ := Public(in)
			if l := links(out); len(l) != 0 {
				t.Errorf("%v survived in %q", l, out)
			}
		})
	}
	if out, _ := Public("`[a](https://evil.example/x)` stays as written"); !strings.HasPrefix(out, "`[a](https://evil.example/x)`") {
		t.Errorf("a code span is left alone: %q", out)
	}
}

// A mention pings a person and a cross-reference writes a backlink on another issue: both come out
// in code spans, where GitHub renders neither. A heading, a plain word and an issue-less "#" stay.
func TestPublicDefusesMentionsAndReferences(t *testing.T) {
	in := "## What is wrong\nping @smana and (@org/team), see #123, Smana/other#7 and fix#9.\n" +
		"## What it should be\n@bob: #4.\n## Acceptance check\nmail a@b.example, `@kept` and #1 stay, C# and 50% too."
	out, _ := Public(in)
	for _, want := range []string{"`@smana`", "`@org/team`", "`#123`", "`Smana/other#7`", "`@bob`", "`#4`", "`#1`", "`@kept`"} {
		if !strings.Contains(out, want) {
			t.Errorf("%s is not in a code span: %q", want, out)
		}
	}
	for _, bad := range []string{" @smana", "(@org", " #123", "other#7 ", "\n@bob", " #4."} {
		if strings.Contains(out, bad) {
			t.Errorf("%q survived live: %q", bad, out)
		}
	}
	if !strings.Contains(out, "## What is wrong") || !strings.Contains(out, "C# and 50% too") {
		t.Errorf("a heading and a plain # are left alone: %q", out)
	}
}
