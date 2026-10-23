// SPDX-License-Identifier: Apache-2.0

package sanitize

import (
	"strings"
	"testing"
)

const goodBody = "## What is wrong\nx\n## What it should be\ny\n## Acceptance check\nz"

func TestCheckDraft(t *testing.T) {
	for name, c := range map[string]struct {
		title, body string
		want        string // error prefix, "" for none
	}{
		"good":             {"docs(agents): the link is dead", goodBody, ""},
		"no area":          {"the link is dead", goodBody, "invalid_draft: the title"},
		"flag-like":        {"-x: y", goodBody, "invalid_draft: the title"},
		"two lines":        {"docs: a\nb", goodBody, "invalid_draft: the title"},
		"long title":       {"docs: " + strings.Repeat("a", 260), goodBody, "invalid_draft: the title"},
		"long body":        {"docs: a", goodBody + strings.Repeat("a", 17<<10), "invalid_draft: the body is at most"},
		"missing section":  {"docs: a", "## What is wrong\nx\n## What it should be\ny", "invalid_draft: the body needs"},
		"sections reorder": {"docs: a", "## What it should be\ny\n## What is wrong\nx\n## Acceptance check\nz", "invalid_draft: the body needs"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := CheckDraft(c.title, c.body)
			if (c.want == "") != (err == nil) || (err != nil && !strings.HasPrefix(err.Error(), c.want)) {
				t.Fatalf("%v, want %q", err, c.want)
			}
		})
	}
}

func TestCheckDraftStripped(t *testing.T) {
	d, err := CheckDraft("docs(a\u200b): b\u200b", strings.Replace(goodBody, "x", "x\u200b\u0007", 1))
	if err != nil || d.Stripped != 4 || d.Title != "docs(a): b" || strings.ContainsAny(d.Body, "\u200b\u0007") {
		t.Fatalf("%+v %v", d, err)
	}
}
