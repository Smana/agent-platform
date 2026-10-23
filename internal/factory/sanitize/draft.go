// SPDX-License-Identifier: Apache-2.0

package sanitize

import (
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"
)

// An issue draft's bounds: the factory-handoff skill's template (references/issue-template.md).
const (
	maxDraftTitle = 256 // runes
	maxDraftBody  = 16 << 10
)

// draftTitleRE is the template's title: `<area>: <what is wrong>`, as `docs(agents): …`. A first
// letter or digit also keeps the title from reading as one of gh's flags.
var draftTitleRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*(\([A-Za-z0-9._/-]+\))?: \S`)

// draftSections are the template's headings, in order.
var draftSections = []string{"## What is wrong", "## What it should be", "## Acceptance check"}

// Draft is a checked issue draft: hidden characters stripped, the template held.
type Draft struct {
	Title, Body string
	// Stripped counts the hidden and control characters removed (F8): what is filed is what is shown.
	Stripped int
}

// CheckDraft holds a draft to the issue template: issue_draft's rules, which the factory's intake
// api shares. Its errors are the messages issue_draft shows, each starting invalid_draft. It does
// not look for secrets: the broker's redactor does, before a draft leaves it.
func CheckDraft(title, body string) (Draft, error) {
	t, tr := Hidden(strings.TrimSpace(title))
	b, br := Hidden(strings.TrimSpace(body))
	if strings.Contains(t, "\n") || utf8.RuneCountInString(t) > maxDraftTitle || !draftTitleRE.MatchString(t) {
		return Draft{}, errors.New("invalid_draft: the title is one line, `<area>: <what is wrong>`, at most 256 characters")
	}
	if len(b) > maxDraftBody {
		return Draft{}, errors.New("invalid_draft: the body is at most 16 KiB")
	}
	if missing := missingSections(b); len(missing) > 0 {
		return Draft{}, errors.New("invalid_draft: the body needs the template's sections, in order: " +
			strings.Join(draftSections, ", ") + "; missing or out of order: " + strings.Join(missing, ", "))
	}
	return Draft{Title: t, Body: b, Stripped: tr.Invisible + tr.Control + br.Invisible + br.Control}, nil
}

// missingSections are the template's headings body lacks, in order: each must start a line after
// the one before it.
func missingSections(body string) []string {
	next := 0
	for line := range strings.SplitSeq(body, "\n") {
		if next < len(draftSections) && strings.TrimSpace(line) == draftSections[next] {
			next++
		}
	}
	return draftSections[next:]
}
