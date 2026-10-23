// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/factory/sanitize"
	"github.com/Smana/agent-platform/internal/mcpwire"
)

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
	d, err := s.checkDraft(ctx, a.Title, a.Body)
	if err != nil {
		return nil, err
	}
	return issueDraft{Repo: a.Repo, Title: d.Title, Body: d.Body, Stripped: d.Stripped}, nil
}

// checkDraft is issue_draft's whole check: the template (sanitize.CheckDraft), then the redactor,
// failing closed without one. Its refusals are toolErrors the model reads as they are.
func (s *Server) checkDraft(ctx context.Context, title, body string) (sanitize.Draft, error) {
	d, err := sanitize.CheckDraft(title, body)
	if err != nil {
		return sanitize.Draft{}, toolError(err.Error())
	}
	if s.Actor == nil || s.Actor.Redactor == nil {
		return sanitize.Draft{}, errors.New("humanapi: no redactor") // fail closed: a secret would go unseen
	}
	_, rules, err := s.Actor.Redactor.Payload(ctx, envelope.Must(map[string]string{"title": d.Title, "body": d.Body}))
	if err != nil {
		return sanitize.Draft{}, err
	}
	if len(rules) > 0 {
		return sanitize.Draft{}, toolError("invalid_draft: it holds what looks like a secret (" + strings.Join(rules, ", ") +
			"): remove it, since the factory's agents read the issue in full")
	}
	return d, nil
}
