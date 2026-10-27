// SPDX-License-Identifier: Apache-2.0

package roomview

import (
	"bytes"
	"strings"
	"testing"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	extast "github.com/yuin/goldmark/extension/ast"
	"github.com/yuin/goldmark/text"
)

func parse(md string) (ast.Node, []byte) {
	src := []byte(md)
	g := goldmark.New(goldmark.WithExtensions(extension.GFM))
	return g.Parser().Parse(text.NewReader(src)), src
}

// plain is the text a node shows, without what sits in a code span: a code span is the inert place
// for agent text.
func plain(n ast.Node, src []byte) string {
	var b strings.Builder
	_ = ast.Walk(n, func(c ast.Node, enter bool) (ast.WalkStatus, error) {
		switch t := c.(type) {
		case *ast.CodeSpan:
			return ast.WalkSkipChildren, nil
		case *ast.Text:
			if enter {
				b.Write(t.Segment.Value(src))
			}
		case *ast.String:
			if enter {
				b.Write(t.Value)
			}
		}
		return ast.WalkContinue, nil
	})
	return unescape(b.String())
}

// unescape reads a backslash before an ASCII punctuation character as the character alone, as
// CommonMark does: goldmark keeps the backslash in the text's source segment.
func unescape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && strings.IndexByte("!\"#$%&'()*+,-./:;<=>?@[\\]^_`{|}~", s[i+1]) >= 0 {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// assertInert fails when md, as GitHub Flavored Markdown, holds a link whose destination is off the
// allowlist, raw HTML, agent text in a structural node outside a code span, or a control byte.
func assertInert(t *testing.T, md string, rooms string, untrusted []string) {
	t.Helper()
	for i := 0; i < len(md); i++ {
		if md[i] < 0x20 && md[i] != '\n' {
			t.Errorf("byte %#x at %d of %q", md[i], i, md)
		}
	}
	o := Options{Rooms: rooms}
	root, src := parse(md)
	_ = ast.Walk(root, func(n ast.Node, enter bool) (ast.WalkStatus, error) {
		if !enter {
			return ast.WalkContinue, nil
		}
		switch k := n.(type) {
		case *ast.Link:
			if !allowed(string(k.Destination), o) {
				t.Errorf("link to %q is off the allowlist", k.Destination)
			}
		case *ast.AutoLink:
			if k.AutoLinkType != ast.AutoLinkURL || !allowed(string(k.URL(src)), o) {
				t.Errorf("autolink %q is off the allowlist", k.URL(src))
			}
		case *ast.RawHTML, *ast.HTMLBlock:
			t.Errorf("raw HTML in %q", md)
		}
		switch n.(type) {
		case *ast.Heading, *ast.List, *extast.Table, *ast.Emphasis, *extast.Strikethrough, *ast.Link:
			got := plain(n, src)
			for _, u := range untrusted {
				if u != "" && strings.Contains(got, u) {
					t.Errorf("%T holds untrusted %q outside a code span: %q", n, u, got)
				}
			}
		}
		return ast.WalkContinue, nil
	})
}

// codeSpans lists the text of every code span in md.
func codeSpans(md string) []string {
	root, src := parse(md)
	var out []string
	_ = ast.Walk(root, func(n ast.Node, enter bool) (ast.WalkStatus, error) {
		if c, ok := n.(*ast.CodeSpan); ok && enter {
			var b bytes.Buffer
			for ch := c.FirstChild(); ch != nil; ch = ch.NextSibling() {
				if t, ok := ch.(*ast.Text); ok {
					b.Write(t.Segment.Value(src))
				}
			}
			out = append(out, b.String())
		}
		return ast.WalkContinue, nil
	})
	return out
}
