// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"encoding/json"
	"strings"
	"testing"
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
