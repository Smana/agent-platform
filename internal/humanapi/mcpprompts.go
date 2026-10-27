// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"unicode"

	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/mcpwire"
)

// promptArg is one optional prompt argument.
type promptArg struct{ name, description string }

// mcpPrompt is one /mcp prompt: a canned user message the client offers as a slash command.
type mcpPrompt struct {
	name, description string
	args              []promptArg
	text              func(args map[string]string) string
}

// orElse is v, or def when v is empty.
func orElse(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// refText is the developer's room reference made safe to quote in a code span: no backtick or
// control character, at most 80 runes.
func refText(s string) string {
	out := []rune{}
	for _, r := range s {
		if r == '`' || unicode.IsControl(r) {
			continue
		}
		if out = append(out, r); len(out) == 80 {
			break
		}
	}
	return strings.TrimSpace(string(out))
}

// mcpPrompts are the prompts a human's roomctl is offered; a sponsored principal has none.
var mcpPrompts = []mcpPrompt{
	{
		name: "handoff", description: "Hand a side task to the agent factory as one issue.",
		args: []promptArg{{"what", "the side task; default: the one just discussed"}},
		text: func(a map[string]string) string {
			return "Draft one issue for " + orElse(a["what"], "the side task we just discussed") +
				" in the template's three sections (`## What is wrong`, `## What it should be`, `## Acceptance check`). " +
				"Check it with `issue_draft`, fix what it reports, then call `issue_file`. " +
				"Show each tool's text as it is, and never apply a label."
		},
	},
	{
		name: "status", description: "Show one room's card.",
		args: []promptArg{{"room", "a room id, #<issue>, PR #<n> or an issue number; default: your most recently updated room"}},
		text: func(a map[string]string) string {
			if r := refText(a["room"]); r != "" {
				return "Call `room_summary` with room `" + r + "` (as written: roomctl resolves #<issue>, PR #<n> and numbers, and says when several rooms match; ask the developer then, never guess), and show its text as it is."
			}
			return "Find the caller's most recently updated room (from `rooms_list`), call `room_summary` for it, and show its text as it is."
		},
	},
	{
		name: "inbox", description: "Show the rooms that need you, then your other rooms.",
		text: func(map[string]string) string {
			return "Call `rooms_list` with `needsMe`, then with `mine`, and show each text as it is."
		},
	},
}

// mcpPromptsList is prompts/list: a sponsored principal has no prompts, so its list is empty.
func mcpPromptsList(w http.ResponseWriter, req mcpwire.Request, p authn.Principal) {
	list := []map[string]any{}
	if !p.Sponsored {
		for _, pr := range mcpPrompts {
			args := []map[string]any{}
			for _, a := range pr.args {
				args = append(args, map[string]any{"name": a.name, "description": a.description, "required": false})
			}
			list = append(list, map[string]any{"name": pr.name, "description": pr.description, "arguments": args})
		}
	}
	mcpwire.Write(w, req.ID, map[string]any{"prompts": list}, nil)
}

// mcpPromptsGet is prompts/get: the named prompt's message, its arguments filled in. An unknown
// prompt or argument is invalid params.
func mcpPromptsGet(w http.ResponseWriter, req mcpwire.Request, p authn.Principal) {
	var g struct {
		Name      string            `json:"name"`
		Arguments map[string]string `json:"arguments"`
	}
	bad := func(msg string) {
		mcpwire.Write(w, req.ID, nil, &mcpwire.Error{Code: mcpwire.InvalidParams, Message: msg})
	}
	if len(req.Params) == 0 || json.Unmarshal(req.Params, &g) != nil || g.Name == "" {
		bad("prompts/get takes {name, arguments}")
		return
	}
	i := slices.IndexFunc(mcpPrompts, func(pr mcpPrompt) bool { return pr.name == g.Name })
	if i < 0 || p.Sponsored {
		bad("unknown prompt: " + g.Name)
		return
	}
	pr := mcpPrompts[i]
	for k := range g.Arguments {
		if !slices.ContainsFunc(pr.args, func(a promptArg) bool { return a.name == k }) {
			bad("unknown argument: " + k)
			return
		}
	}
	mcpwire.Write(w, req.ID, map[string]any{"description": pr.description, "messages": []map[string]any{
		{"role": "user", "content": map[string]string{"type": "text", "text": pr.text(g.Arguments)}}}}, nil)
}
