// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/factory/sanitize"
	"github.com/Smana/agent-platform/internal/mcpwire"
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

// issueDraft is issue_draft's result: what roomctl mcp's issue_file shows the developer, then files.
type issueDraft struct {
	Repo  string `json:"repo"`
	Title string `json:"title"`
	Body  string `json:"body"`
	// Stripped counts the hidden and control characters removed (F8): what is filed is what is shown.
	Stripped int `json:"stripped"`
}

// issue_draft validates a factory issue in the skill's shape: hidden characters stripped, the
// template's title and sections, no secret. It files nothing: issue_file, roomctl mcp's own tool,
// files it with the developer's gh after they confirm (F3).
func (s *Server) issueDraftTool(ctx context.Context, _ authn.Principal, args json.RawMessage) (any, error) {
	var a struct {
		Repo  string `json:"repo"`
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	if mcpwire.DecodeArgs(args, &a) != nil || !repositoryRE.MatchString(a.Repo) {
		return nil, toolError("invalid_arguments: repo is owner/name; title and body are strings")
	}
	title, tr := sanitize.Hidden(strings.TrimSpace(a.Title))
	body, br := sanitize.Hidden(strings.TrimSpace(a.Body))
	if strings.Contains(title, "\n") || utf8.RuneCountInString(title) > maxDraftTitle || !draftTitleRE.MatchString(title) {
		return nil, toolError("invalid_draft: the title is one line, `<area>: <what is wrong>`, at most 256 characters")
	}
	if len(body) > maxDraftBody {
		return nil, toolError("invalid_draft: the body is at most 16 KiB")
	}
	if missing := missingSections(body); len(missing) > 0 {
		return nil, toolError("invalid_draft: the body needs the template's sections, in order: " +
			strings.Join(draftSections, ", ") + "; missing or out of order: " + strings.Join(missing, ", "))
	}
	if s.Actor == nil || s.Actor.Redactor == nil {
		return nil, errors.New("humanapi: no redactor") // fail closed: a secret would go unseen
	}
	_, rules, err := s.Actor.Redactor.Payload(ctx, envelope.Must(map[string]string{"title": title, "body": body}))
	if err != nil {
		return nil, err
	}
	if len(rules) > 0 {
		return nil, toolError("invalid_draft: it holds what looks like a secret (" + strings.Join(rules, ", ") +
			"): remove it, since the factory's agents read the issue in full")
	}
	return issueDraft{Repo: a.Repo, Title: title, Body: body, Stripped: tr.Invisible + tr.Control + br.Invisible + br.Control}, nil
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
