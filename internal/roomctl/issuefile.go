// SPDX-License-Identifier: Apache-2.0

package roomctl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/Smana/agent-platform/internal/mcpwire"
)

// issueFileTool is roomctl mcp's own tool: the broker never files an issue (F3).
const issueFileTool = "issue_file"

// IssueFiler files a confirmed draft as the developer and returns the issue's URL. GH is the one
// roomctl mcp uses.
type IssueFiler func(ctx context.Context, repo, title, body string) (string, error)

// Labeler puts a label on an issue as the developer. GHLabel is the one roomctl mcp uses.
type Labeler func(ctx context.Context, repo string, issue int, label string) error

// readyLabel is the label that starts the factory: the developer's own, never run_create.
const readyLabel = "factory/ready"

// issueFileEntry is issue_file's tools/list entry.
var issueFileEntry = map[string]any{
	"name": issueFileTool,
	"description": "File a factory issue as the developer, with their gh, once they confirm it in a prompt this tool shows them. " +
		"The draft is checked by issue_draft first. It labels nothing itself: the form offers to start the factory only when issue_draft says canStart, and the label is the developer's, applied with their gh.",
	"inputSchema": json.RawMessage(`{"type":"object","additionalProperties":false,"required":["repo","title","body"],"properties":{"repo":{"type":"string"},"title":{"type":"string"},"body":{"type":"string"}}}`),
}

// draftShape is the broker's issue_draft result, which issue_file files as is.
type draftShape struct {
	Repo        string `json:"repo"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	CanStart    bool   `json:"canStart"`    // absent from an older broker: false
	CanStartWhy string `json:"canStartWhy"` // why not, when it is false
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
	elicit := r.forms
	r.mu.Unlock()
	if !elicit || r.File == nil {
		return mcpwire.ToolError("needs_confirmation: issue_file asks the developer to confirm, and this client cannot ask (no elicitation); " +
			"show them the draft and let them file it with gh issue create")
	}
	draft, refused := r.draft(ctx, args)
	if refused != nil {
		return refused
	}
	offer := draft.CanStart && r.Label != nil
	// A default, or Claude Code shows the required toggle as unset with "This field is required"
	// until it is toggled; false keeps filing an explicit yes.
	props := map[string]any{"file": map[string]any{"type": "boolean", "title": "File this issue", "default": false, "description": "Filed with your gh, as you"}}
	tail := "It gets no label: label it " + readyLabel + " yourself to start the factory."
	if offer {
		props["start"] = map[string]any{"type": "boolean", "title": "Start the factory now", "default": false,
			"description": "Also labels it " + readyLabel + " with your gh, which starts the factory"}
		tail = "Choose \"start\" to also label it " + readyLabel + " with your gh, which starts the factory."
	} else if why := OneLine(draft.CanStartWhy); why != "" {
		tail = "The factory cannot be started from here (" + why + "). It gets no label: label it " + readyLabel + " yourself to start the factory."
	}
	ans, err := r.ask(ctx, "elicitation/create", map[string]any{
		"message":         fmt.Sprintf("File this issue on %s as you, with gh? %s\n\n%s\n\n%s", draft.Repo, tail, draft.Title, draft.Body),
		"requestedSchema": map[string]any{"type": "object", "required": []string{"file"}, "properties": props},
	})
	if err != nil {
		return mcpwire.ToolError("not_filed: the developer was not asked: " + err.Error())
	}
	var res struct {
		Action  string                     `json:"action"`
		Content map[string]json.RawMessage `json:"content"` // a client may return it partial
	}
	if len(ans.Error) > 0 || json.Unmarshal(ans.Result, &res) != nil {
		return mcpwire.ToolError("not_filed: the client could not ask the developer")
	}
	file, _ := formBool(res.Content["file"])
	if res.Action != "accept" || !file {
		out, _ := mcpwire.ToolResult(map[string]any{"filed": false, "why": "the developer did not confirm (" + res.Action + ")"})
		return out
	}
	start, chosen := formBool(res.Content["start"])
	if offer && !chosen {
		r.log().Warn("issue_file: the form answer has no usable start; treated as not started", "start", string(res.Content["start"]))
	}
	url, err := r.File(ctx, draft.Repo, draft.Title, draft.Body)
	if err != nil {
		return mcpwire.ToolError("not_filed: " + err.Error())
	}
	if !offer || !start { // a start the form never offered is ignored
		next := "label it " + readyLabel + " to start the factory: your decision"
		if offer && !chosen {
			next = "start was not chosen (the form came back without it), so the factory is not started: ask the developer whether to start it, and label it " + readyLabel + " only on their yes"
		}
		out, _ := mcpwire.ToolResult(map[string]any{"filed": true, "url": url, "started": false, "next": next})
		return out
	}
	return r.start(ctx, draft.Repo, url)
}

// formBool reads a form field: a JSON boolean, or the strings "true" and "false" some clients send.
// ok is false when the field is absent, null or neither, which callers treat as false.
func formBool(raw json.RawMessage) (v, ok bool) {
	var b bool
	if json.Unmarshal(raw, &b) == nil && len(raw) > 0 && string(raw) != "null" {
		return b, true
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		switch strings.ToLower(strings.TrimSpace(str)) {
		case "true":
			return true, true
		case "false":
			return false, true
		}
	}
	return false, false
}

// start labels the issue just filed and waits for its room. A failure files without the label and
// says so: it is never retried, since the developer can label the issue themselves.
func (r *Relay) start(ctx context.Context, repo, url string) any {
	result := map[string]any{"filed": true, "url": url, "started": false}
	n, err := issueNumber(url, repo)
	if err == nil {
		err = r.Label(ctx, repo, n, readyLabel)
	}
	if err != nil {
		result["why"] = "filed without the label, so not started: " + OneLine(err.Error()) + "; label it " + readyLabel + " yourself"
		out, _ := mcpwire.ToolResult(result)
		return out
	}
	result["started"] = true
	id := r.awaitRoom(ctx, repo, n)
	if id == "" {
		result["why"] = "not started yet: the factory's comment on the issue says why"
		out, _ := mcpwire.ToolResult(result)
		return out
	}
	result["room"] = id
	out, _ := mcpwire.ToolResult(result)
	if text, sc, isErr, err := r.callBroker(ctx, "room_summary", map[string]string{"room": id}); err == nil && !isErr {
		if card, ok := cardText(viewed("room_summary", cardResult(text, sc), r.URL, time.Now())); ok {
			out["content"] = []map[string]string{{"type": "text", "text": card}}
		}
	}
	return out
}

// awaitRoom polls rooms_list every 5 s for the room of the issue, up to RoomWait; "" if none came.
func (r *Relay) awaitRoom(ctx context.Context, repo string, issue int) string {
	wait := r.RoomWait
	if wait <= 0 {
		wait = roomWait
	}
	ctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	for {
		if _, sc, isErr, err := r.callBroker(ctx, "rooms_list", map[string]any{}); err == nil && !isErr {
			var list struct {
				Rooms []struct {
					ID         string `json:"id"`
					Repository string `json:"repository"`
					Issue      int    `json:"issue"`
				} `json:"rooms"`
			}
			if json.Unmarshal(sc, &list) == nil {
				for _, row := range list.Rooms {
					if strings.EqualFold(row.Repository, repo) && row.Issue == issue && row.ID != "" {
						return row.ID
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return ""
		case <-r.after(defaultPoll):
		}
	}
}

// cardResult rebuilds a room_summary tools/call result from what callBroker returned, for viewed.
func cardResult(text string, structured json.RawMessage) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"content": []map[string]string{{"type": "text", "text": text}}, "structuredContent": structured})
	return b
}

// cardText is the first text of a tools/call result.
func cardText(res json.RawMessage) (string, bool) {
	var out struct {
		Content []struct{ Text string } `json:"content"`
	}
	if json.Unmarshal(res, &out) != nil || len(out.Content) == 0 || out.Content[0].Text == "" {
		return "", false
	}
	return out.Content[0].Text, true
}

// issueURL is the shape of the URL gh prints for an issue.
var issueURL = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9-]+/[A-Za-z0-9._-]+)/issues/([1-9][0-9]{0,8})$`)

// issueNumber is the number in https://github.com/<repo>/issues/<n>; any other shape, or another
// repository, is refused: the number becomes gh's argument.
func issueNumber(url, repo string) (int, error) {
	m := issueURL.FindStringSubmatch(strings.TrimSpace(url))
	if m == nil || !strings.EqualFold(m[1], repo) { // gh prints the canonical case
		return 0, fmt.Errorf("gh printed %q, not an issue of %s", OneLine(url), repo)
	}
	return strconv.Atoi(m[2])
}

// GHLabel labels the issue with the developer's gh: each value joined to its flag, never a shell.
func GHLabel(ctx context.Context, repo string, issue int, label string) error {
	ctx, cancel := context.WithTimeout(ctx, ghTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", "issue", "edit", strconv.Itoa(issue), "--repo="+repo, "--add-label="+label) //nolint:gosec // gh, with a checked repository, a number and a fixed label, never through a shell
	var errOut bytes.Buffer
	cmd.Stderr = &capped{b: &errOut}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gh issue edit: %w: %s", err, OneLine(errOut.String()))
	}
	return nil
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

// ghTimeout bounds one gh call.
var ghTimeout = 60 * time.Second

// maxGHOutput bounds what gh's stdout and stderr keep.
const maxGHOutput = 4 << 10

// GH files the issue with the developer's gh: their identity, the body on stdin, never a label.
func GH(ctx context.Context, repo, title, body string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, ghTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", ghArgs(repo, title)...) //nolint:gosec // gh, with a checked repository and title, never through a shell
	cmd.Stdin = strings.NewReader(body)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &capped{b: &out}, &capped{b: &errOut}
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("gh issue create: %w: %s", err, OneLine(errOut.String()))
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
