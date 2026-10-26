// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Smana/agent-platform/internal/fanout"
	"github.com/Smana/agent-platform/internal/repoaccess"
)

func TestMCPIssueDraft(t *testing.T) {
	e := setup(t, mcpOn, actorOn(&actLog{}))
	body := "## What is wrong\ndocs/a.md:3 links\u200b a removed runbook\n\n## What it should be\nthe new one\n\n## Acceptance check\nhack/docs.sh passes\n"
	got := call(t, e, viaRoomctl("dev"), "issue_draft", map[string]any{"repo": "Smana/a", "title": "docs(agents): a dead link", "body": body})
	var d issueDraft
	if got.isError || json.Unmarshal(got.structured, &d) != nil || d.Stripped != 1 || strings.Contains(d.Body, "\u200b") || d.Title != "docs(agents): a dead link" {
		t.Fatalf("%+v", got)
	}
	for name, c := range map[string]struct {
		title, body, want string
	}{
		"a title off the template": {"fix the docs", body, "invalid_draft: the title"},
		"a flag for a title":       {"--label factory/ready: x", body, "invalid_draft: the title"},
		"a missing section":        {"docs: x", "## What is wrong\nx\n## Acceptance check\ny", "missing or out of order: ## What it should be, ## Acceptance check"},
		"a secret":                 {"docs: x", body + "token SECRET", "invalid_draft: it holds what looks like a secret (test)"},
	} {
		t.Run(name, func(t *testing.T) {
			got := call(t, e, viaRoomctl("dev"), "issue_draft", map[string]any{"repo": "Smana/a", "title": c.title, "body": c.body})
			if !got.isError || !strings.Contains(got.text, c.want) {
				t.Fatalf("%+v", got)
			}
		})
	}
}

// issue_draft says whether the caller may start the factory on the repository (R11): the
// repository is in human.mcp.sessionStart, and the caller writes to it on GitHub.
func TestIssueDraftCanStart(t *testing.T) {
	listed := func(s *Server, _ *fanout.Hub, _ *hubView) { s.SessionStart = map[string]bool{"smana/a": true} }
	tests := []struct {
		name    string
		w       *gitHubWorld
		opts    []option
		caller  http.Header
		repo    string
		want    bool
		wantWhy string
	}{
		{"listed, linked, write", &gitHubWorld{writes: map[string][]string{"Smana/a": {"dev"}}}, []option{listed}, viaRoomctl("dev"), "Smana/a", true, ""},
		{"not listed", &gitHubWorld{writes: map[string][]string{"Smana/b": {"dev"}}}, []option{listed}, viaRoomctl("dev"), "Smana/b", false, "off"},
		{"listed, Identity nil", &gitHubWorld{}, []option{listed, func(s *Server, _ *fanout.Hub, _ *hubView) { s.Identity = nil }}, viaRoomctl("dev"), "Smana/a", false, "off"},
		{"listed, unlinked", &gitHubWorld{unlinked: map[string]bool{"dev": true}}, []option{listed}, viaRoomctl("dev"), "Smana/a", false, "not_linked"},
		{"listed, read only", &gitHubWorld{}, []option{listed}, viaRoomctl("dev"), "Smana/a", false, "no_write"},
		{"listed, the App cannot see it", &gitHubWorld{unseen: map[string]bool{"Smana/a": true}}, []option{listed}, viaRoomctl("dev"), "Smana/a", false, "no_write"},
		{"listed, the lookup fails", &gitHubWorld{down: true}, []option{listed}, viaRoomctl("dev"), "Smana/a", false, "unverified"},
		{"nil Access", &gitHubWorld{}, []option{listed, func(s *Server, _ *fanout.Hub, _ *hubView) { s.Access = nil }}, viaRoomctl("dev"), "Smana/a", false, "off"},
		{"empty SessionStart", &gitHubWorld{writes: map[string][]string{"Smana/a": {"dev"}}}, []option{func(s *Server, _ *fanout.Hub, _ *hubView) { s.SessionStart = map[string]bool{} }}, viaRoomctl("dev"), "Smana/a", false, "off"},
		{"nil SessionStart", &gitHubWorld{writes: map[string][]string{"Smana/a": {"dev"}}}, nil, viaRoomctl("dev"), "Smana/a", false, "off"},
		{"the write check itself says no repository", &gitHubWorld{}, []option{listed, func(s *Server, _ *fanout.Hub, _ *hubView) {
			s.Access.Perm = func(context.Context, string, string, string) (string, error) {
				return "", fmt.Errorf("installation: %w", repoaccess.ErrNoRepository)
			}
		}}, viaRoomctl("dev"), "Smana/a", false, "no_write"},
		{"the list is matched case-insensitively", &gitHubWorld{writes: map[string][]string{"Smana/a": {"dev"}}}, []option{func(s *Server, _ *fanout.Hub, _ *hubView) { s.SessionStart = map[string]bool{"smana/a": true} }}, viaRoomctl("dev"), "Smana/a", true, ""},
		{"a sponsored caller", &gitHubWorld{writes: map[string][]string{"Smana/a": {"slack-app"}}}, []option{listed, withSponsored(sponsoredP)}, bearer("slack-app"), "Smana/a", false, "off"},
	}
	body := "## What is wrong\nx\n\n## What it should be\ny\n\n## Acceptance check\nz\n"
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.w.now = time.Now()
			if tc.w.unlinked == nil {
				tc.w.unlinked = map[string]bool{}
			}
			if tc.w.inactive == nil {
				tc.w.inactive = map[string]bool{}
			}
			e := setup(t, append([]option{mcpOn, actorOn(&actLog{}), tc.w.option()}, tc.opts...)...)
			got := call(t, e, tc.caller, "issue_draft", map[string]any{"repo": tc.repo, "title": "docs(agents): a dead link", "body": body})
			var d issueDraft
			if got.isError || json.Unmarshal(got.structured, &d) != nil || d.CanStart != tc.want || d.CanStartWhy != tc.wantWhy {
				t.Fatalf("%+v: %+v", got, d)
			}
		})
	}
}
