// SPDX-License-Identifier: Apache-2.0

package roomview

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/yuin/goldmark/ast"
	extast "github.com/yuin/goldmark/extension/ast"
)

const rooms = "https://rooms.example"

func TestClean(t *testing.T) {
	for name, tc := range map[string]struct{ in, want string }{
		"hidden characters": {"a\u200bb\u202ec\x1b[2J", "abc[2J"},
		"whitespace":        {"one\n\n two\t three", "one two three"},
		"newline":           {"a\nb", "a b"},
		"hidden token":      {"a \u200b b", "a b"},
		"hidden only":       {"\u200b \u202e", ""},
		"long":              {strings.Repeat("é", 200), strings.Repeat("é", 160) + "…"},
	} {
		t.Run(name, func(t *testing.T) {
			if got := clean(tc.in); got != tc.want {
				t.Fatalf("clean(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestCodeSpanOutrunsBackticks(t *testing.T) {
	if got := code("a``b", false); got != "```a``b```" {
		t.Fatalf("code = %q", got)
	}
	if got := code("`x", false); !strings.Contains(got, " `x") {
		t.Fatalf("code = %q, want a space before `x", got)
	}
	// clean() leaves no space at either edge, so code() never meets the one case where CommonMark
	// strips a space the span meant: these round trips are the whole contract.
	for _, in := range []string{"a``b", "`x", "x`", "a `b` c", "plain", "```z", "`", "``", "", "`a`"} {
		got := codeSpans("x " + code(in, false) + " y")
		want := in
		if in == "" {
			want = "  " // a space-only span is not stripped, and "``" alone would be no span at all
		}
		if len(got) != 1 || got[0] != want {
			t.Errorf("%q reads back as %q", in, got)
		}
	}
}

func TestTableCellHasNoPipe(t *testing.T) {
	for _, in := range []string{"a|b", `a\|b`, `a\\|b`, "|", "x|`y`|z", `x\|[y](https://evil)`} {
		c := code(in, true)
		if strings.Contains(c, "|") {
			t.Errorf("code(%q, true) = %q: no pipe may reach a cell, whatever precedes it", in, c)
		}
		md := "| h1 | h2 |\n|---|---|\n| " + c + " | y |\n"
		root, _ := parse(md)
		cells := 0
		_ = ast.Walk(root, func(n ast.Node, enter bool) (ast.WalkStatus, error) {
			if _, ok := n.(*extast.TableCell); ok && enter {
				cells++
			}
			return ast.WalkContinue, nil
		})
		if cells != 4 { // 2 header cells, 2 body cells
			t.Errorf("%q: %d cells, want 4", c, cells)
		}
		if want := strings.ReplaceAll(in, "|", "\u2223"); len(codeSpans(md)) != 1 || codeSpans(md)[0] != want {
			t.Errorf("%q: the cell's code span reads %q, want %q", c, codeSpans(md), want)
		}
		assertInert(t, md, rooms, []string{"evil"})
	}
	if got := escapeTitle(`a|b`, true); strings.Contains(got, "|") {
		t.Errorf("escapeTitle in a cell kept a pipe: %q", got)
	}
}

func TestTitleWordsThatAutolinkGoInCodeSpans(t *testing.T) {
	for _, w := range []string{"https://evil.example", "www.evil.example", "a@evil.example", "mailto:a@evil.example",
		"(www.evil.example)", "*www.evil.example*", "_www.evil.example_", "~www.evil.example~", "see:www.evil.example", "https://evil", "a@b.c"} {
		md := "a title with " + escapeTitle(w, false) + " in it\n"
		assertInert(t, md, rooms, nil)
		if got := codeSpans(md); len(got) != 1 || got[0] != w {
			t.Errorf("%q: code spans %q, want the word alone", w, got)
		}
	}
}

// A title with several words keeps only the dangerous one in a code span, and no link node renders.
func TestTitleWithAutolinkingWordsRendersNoLink(t *testing.T) {
	md := "see " + escapeTitle("see https://evil (www.evil.example) a@b.c *www.evil.example*", false) + " end\n"
	assertInert(t, md, rooms, nil)
	root, _ := parse(md)
	links := 0
	_ = ast.Walk(root, func(n ast.Node, enter bool) (ast.WalkStatus, error) {
		switch n.(type) {
		case *ast.Link, *ast.AutoLink:
			links++
		}
		return ast.WalkContinue, nil
	})
	if links != 0 {
		t.Errorf("%d link nodes in %q", links, md)
	}
}

func TestTitleEscapesMarkdown(t *testing.T) {
	in := "# x *y* [z](w) <b>v</b> a|b ~~s~~ _u_ &amp; 1. - +"
	md := escapeTitle(in, false) + "\n"
	assertInert(t, md, rooms, []string{"x", "y", "z"})
	root, src := parse(md)
	if root.ChildCount() != 1 {
		t.Fatalf("%q parses to %d blocks, want one paragraph", md, root.ChildCount())
	}
	p, ok := root.FirstChild().(*ast.Paragraph)
	if !ok {
		t.Fatalf("%q parses to %T, want a paragraph", md, root.FirstChild())
	}
	if got := plain(p, src); got != in {
		t.Fatalf("text %q, want %q", got, in)
	}
}

func TestEscapedTitleInACellKeepsItsColumns(t *testing.T) {
	md := "| h1 | h2 |\n|---|---|\n| " + escapeTitle("a|b *c*", true) + " | y |\n"
	root, _ := parse(md)
	cells := 0
	_ = ast.Walk(root, func(n ast.Node, enter bool) (ast.WalkStatus, error) {
		if _, ok := n.(*extast.TableCell); ok && enter {
			cells++
		}
		return ast.WalkContinue, nil
	})
	if cells != 4 {
		t.Fatalf("%d cells, want 4", cells)
	}
}

func TestRoomsBaseTrailingSlash(t *testing.T) {
	if !allowed("https://rooms.example/r/26zfnuxm", Options{Rooms: rooms + "/"}) {
		t.Fatal("a rooms base with a trailing slash refuses every room link")
	}
}

func TestQuote(t *testing.T) {
	if got := quote("a\x1b[2J \"b\"\u202e\nc"); got != `"a[2J \"b\" c"` {
		t.Fatalf("quote = %s", got)
	}
}

func TestAllowed(t *testing.T) {
	o := Options{Rooms: rooms}
	for _, u := range []string{
		"https://rooms.example/r/26zfnuxm", "https://rooms.example/r/26zfnuxm#01M4A",
		"https://github.com/Smana/cloud-native-ref/issues/42", "https://github.com/Smana/cloud-native-ref/pull/2239",
	} {
		if !allowed(u, o) {
			t.Errorf("%q refused", u)
		}
	}
	for _, u := range []string{
		"https://rooms.example.evil/r/26zfnuxm", "https://rooms.example/admin", "javascript:x",
		"https://rooms.example/r/26zfnuxm)[x](https://evil.example", "https://rooms.example/r/26zfnuxm#a b",
		"https://rooms.example/r/", "http://rooms.example/r/26zfnuxm", "",
	} {
		if allowed(u, o) {
			t.Errorf("%q allowed", u)
		}
	}
	if allowed("https://rooms.example/r/26zfnuxm", Options{}) {
		t.Error("a room URL is allowed with no rooms base")
	}
}

func TestLinkRendersTextAloneOffTheAllowlist(t *testing.T) {
	o := Options{Format: Markdown, Rooms: rooms}
	if got := link("room *x*", "https://rooms.example/r/26zfnuxm", o, false); got != `[room \*x\*](https://rooms.example/r/26zfnuxm)` {
		t.Fatalf("link = %q", got)
	}
	if got := link("room", "https://evil.example", o, false); got != "room" {
		t.Fatalf("link = %q, want the text alone", got)
	}
	o.Format = Text
	if got := link("room", "https://rooms.example/r/26zfnuxm", o, false); got != "room (https://rooms.example/r/26zfnuxm)" {
		t.Fatalf("text link = %q", got)
	}
}

func TestRepoOf(t *testing.T) {
	if got := repoOf("https://github.com/Smana/a/issues/1", "https://github.com/Other/b/pull/2"); got != "Smana/a" {
		t.Fatalf("repoOf = %q", got)
	}
	if got := repoOf("", "https://github.com/Smana/b/pull/2"); got != "Smana/b" {
		t.Fatalf("repoOf = %q, want the first URL on GitHub", got)
	}
	if got := repoOf(); got != "" {
		t.Fatalf("repoOf() = %q", got)
	}
	if got := repoOf("https://evil.example/Smana/a/issues/1"); got != "" {
		t.Fatalf("repoOf = %q, want none for a URL off GitHub", got)
	}
}

// TestHostileFixtureIsInert renders every agent-written string of the hostile summary golden, run
// facts included, the way a view will: in code spans in a table, and the title as escaped text.
func TestHostileFixtureIsInert(t *testing.T) {
	b, err := os.ReadFile("../summary/testdata/hostile.json")
	if err != nil {
		t.Fatal(err)
	}
	var tc struct {
		Want struct {
			Status struct {
				Issue struct{ Title string }
			}
			Runs  []struct{ Role, Verdict string }
			Notes struct{ Items []struct{ Text string } }
		}
	}
	if err := json.Unmarshal(b, &tc); err != nil {
		t.Fatal(err)
	}
	w := tc.Want
	if len(w.Runs) == 0 || !strings.Contains(w.Runs[0].Verdict, `a\|b`) {
		t.Fatalf("the fixture lost its hostile run: %+v", w.Runs)
	}
	var md strings.Builder
	md.WriteString("### room\n\n" + escapeTitle(w.Status.Issue.Title, false) + "\n\n| Role | Verdict |\n|---|---|\n")
	untrusted := []string{"evil", "<b>", "heading", "bold", "img", "impl", "a|b", `a\|b`}
	for _, r := range w.Runs {
		md.WriteString("| " + code(r.Role, true) + " | " + code(r.Verdict, true) + " |\n")
	}
	for _, n := range w.Notes.Items {
		md.WriteString("\n- " + code(n.Text, false) + "\n")
	}
	assertInert(t, md.String(), rooms, untrusted)
	for _, r := range w.Runs {
		for _, in := range []string{r.Role, r.Verdict} {
			found := false
			for _, got := range codeSpans(md.String()) {
				found = found || got == strings.ReplaceAll(clean(in), "|", cellPipe)
			}
			if !found {
				t.Errorf("%q does not read back from a code span (pipes as %s)", clean(in), cellPipe)
			}
		}
	}
}
