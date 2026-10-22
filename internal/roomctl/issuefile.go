// SPDX-License-Identifier: Apache-2.0

package roomctl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"

	"github.com/Smana/agent-platform/internal/mcpwire"
)

// issueFileTool is roomctl mcp's own tool: the broker never files an issue (F3).
const issueFileTool = "issue_file"

// IssueFiler files a confirmed draft as the developer and returns the issue's URL. GH is the one
// roomctl mcp uses.
type IssueFiler func(ctx context.Context, repo, title, body string) (string, error)

// issueFileEntry is issue_file's tools/list entry.
var issueFileEntry = map[string]any{
	"name": issueFileTool,
	"description": "File a factory issue as the developer, with their gh, once they confirm it in a prompt this tool shows them. " +
		"The draft is checked by issue_draft first. It never labels: starting the factory stays the developer's factory/ready label.",
	"inputSchema": json.RawMessage(`{"type":"object","additionalProperties":false,"required":["repo","title","body"],"properties":{"repo":{"type":"string"},"title":{"type":"string"},"body":{"type":"string"}}}`),
}

// withIssueFile adds issue_file to the broker's tools/list result.
func withIssueFile(res json.RawMessage) (json.RawMessage, error) {
	var list map[string]any
	if err := json.Unmarshal(res, &list); err != nil {
		return nil, fmt.Errorf("tools/list: %w", err)
	}
	tools, _ := list["tools"].([]any)
	list["tools"] = append(tools, issueFileEntry)
	return json.Marshal(list)
}

// draftShape is the broker's issue_draft result, which issue_file files as is.
type draftShape struct {
	Repo  string `json:"repo"`
	Title string `json:"title"`
	Body  string `json:"body"`
}

// The draft's repository and title, checked again here: they become gh's arguments.
var (
	repoRE  = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`)
	titleRE = regexp.MustCompile(`^[A-Za-z0-9]`)
)

// issueFile is issue_file: the broker's issue_draft checks the arguments, the client asks the
// developer through an elicitation, and only an accepted one is filed. A client that cannot ask
// is refused: nothing else stands for the developer's yes.
func (r *Relay) issueFile(ctx context.Context, args json.RawMessage) any {
	r.mu.Lock()
	elicit := r.elicit
	r.mu.Unlock()
	if !elicit || r.File == nil {
		return mcpwire.ToolError("needs_confirmation: issue_file asks the developer to confirm, and this client cannot ask (no elicitation); " +
			"show them the draft and let them file it with gh issue create")
	}
	draft, refused := r.draft(ctx, args)
	if refused != nil {
		return refused
	}
	ans, err := r.ask(ctx, "elicitation/create", map[string]any{
		"message": fmt.Sprintf("File this issue on %s as you, with gh? It gets no label: label it factory/ready yourself to start the factory.\n\n%s\n\n%s",
			draft.Repo, draft.Title, draft.Body),
		"requestedSchema": map[string]any{"type": "object", "required": []string{"file"},
			"properties": map[string]any{"file": map[string]any{"type": "boolean", "title": "File this issue", "description": "Filed with your gh, as you, with no label"}}},
	})
	if err != nil {
		return mcpwire.ToolError("not_filed: the developer was not asked: " + err.Error())
	}
	var res struct {
		Action  string `json:"action"`
		Content struct {
			File bool `json:"file"`
		} `json:"content"`
	}
	if len(ans.Error) > 0 || json.Unmarshal(ans.Result, &res) != nil {
		return mcpwire.ToolError("not_filed: the client could not ask the developer")
	}
	if res.Action != "accept" || !res.Content.File {
		out, _ := mcpwire.ToolResult(map[string]any{"filed": false, "why": "the developer did not confirm (" + res.Action + ")"})
		return out
	}
	url, err := r.File(ctx, draft.Repo, draft.Title, draft.Body)
	if err != nil {
		return mcpwire.ToolError("not_filed: " + err.Error())
	}
	out, _ := mcpwire.ToolResult(map[string]any{"filed": true, "url": url, "next": "label it factory/ready to start the factory: your decision"})
	return out
}

// draft asks the broker's issue_draft for the draft to file, or returns the refusal to answer with.
func (r *Relay) draft(ctx context.Context, args json.RawMessage) (draftShape, any) {
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "roomctl-draft", "method": "tools/call",
		"params": map[string]any{"name": "issue_draft", "arguments": args}})
	res, err := r.relay(ctx, raw)
	if err != nil {
		return draftShape{}, mcpwire.ToolError(explainRelay(err))
	}
	var out struct {
		IsError    bool            `json:"isError"`
		Structured json.RawMessage `json:"structuredContent"`
	}
	var d draftShape
	if json.Unmarshal(res, &out) != nil {
		return d, mcpwire.ToolError("unavailable: the broker's draft is unreadable")
	}
	if out.IsError {
		return d, res // the broker's refusal, as the model would read it from issue_draft
	}
	if json.Unmarshal(out.Structured, &d) != nil || !repoRE.MatchString(d.Repo) || !titleRE.MatchString(d.Title) || strings.TrimSpace(d.Body) == "" {
		return d, mcpwire.ToolError("unavailable: the broker's draft is unreadable")
	}
	return d, nil
}

// maxGHOutput bounds what gh's stdout and stderr keep.
const maxGHOutput = 4 << 10

// GH files the issue with the developer's gh: their identity, the body on stdin, never a label.
func GH(ctx context.Context, repo, title, body string) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", ghArgs(repo, title)...) //nolint:gosec // gh, with a checked repository and title, never through a shell
	cmd.Stdin = strings.NewReader(body)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &capped{b: &out}, &capped{b: &errOut}
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("gh issue create: %w: %s", err, oneLine(errOut.String()))
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	return strings.TrimSpace(lines[len(lines)-1]), nil
}

// ghArgs are gh's arguments: each value joined to its flag, so none reads as a flag of its own.
func ghArgs(repo, title string) []string {
	return []string{"issue", "create", "--repo=" + repo, "--title=" + title, "--body-file=-"}
}

// capped keeps the first maxGHOutput bytes written to it and drops the rest.
type capped struct{ b *bytes.Buffer }

func (c *capped) Write(p []byte) (int, error) {
	if room := maxGHOutput - c.b.Len(); room > 0 {
		c.b.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}
