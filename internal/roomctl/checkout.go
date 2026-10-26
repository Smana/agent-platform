// SPDX-License-Identifier: Apache-2.0

package roomctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/Smana/agent-platform/internal/mcpwire"
	"github.com/Smana/agent-platform/internal/roomview"
)

// flagged are the paths whose content a developer's tools run on open or on the next commit: a
// branch that changes one gets a confirmation first (R15). An entry ending in "/" is a prefix.
// The repository's own in-tree core.hooksPath is added at run time. It covers every project MCP
// snippet roomctl setup writes.
var flagged = []string{
	".claude/settings.json", ".claude/settings.local.json", ".opencode/plugins/", ".mcp.json",
	".vscode/tasks.json", ".envrc", "mise.toml", ".mise.toml", "mise.local.toml",
	"opencode.json", "opencode.jsonc", ".codex/config.toml", ".gemini/settings.json",
	".cursor/mcp.json", ".github/mcp.json",
	".husky/", ".githooks/", ".pre-commit-config.yaml", "lefthook.yml", ".lefthook.yml",
}

// maxConfirmed is how many flagged files a form or a refusal names; the rest is counted.
const maxConfirmed = 20

// Checkout is one room_checkout.
type Checkout struct {
	Git                         Git
	Workspace, Room, Repository string
	PR                          int
	// Confirm asks the developer about the flagged files; nil means the client cannot ask.
	Confirm func(ctx context.Context, files []string) (bool, error)
}

// CheckedOut is the worktree the call made or found.
type CheckedOut struct {
	Path   string `json:"path"`
	Branch string `json:"branch"`
	PR     string `json:"pr,omitempty"`
	Reused bool   `json:"reused"`
}

// CheckoutRefusal is a call that did nothing, and why.
type CheckoutRefusal struct{ Why, Fix string }

func (e *CheckoutRefusal) Error() string {
	if e.Fix == "" {
		return e.Why
	}
	return e.Why + "; " + e.Fix
}

func refuse(why, fix string) error { return &CheckoutRefusal{Why: why, Fix: fix} }

// originRE is the origins that are the room's repository: git@github.com:o/n and
// https://github.com/o/n, with or without .git, in any case (L18).
func originRE(repo string) *regexp.Regexp {
	return regexp.MustCompile(`(?i)^(git@github\.com:|https://github\.com/)` + regexp.QuoteMeta(repo) + `(\.git)?$`)
}

// Run checks agent/<room> out into a new worktree beside the workspace, in the spec's order
// (FA8): it never touches the current checkout, never overwrites a path and runs no hook.
func (c Checkout) Run(ctx context.Context) (CheckedOut, error) {
	if !roomIDRE.MatchString(c.Room) || !repoRE.MatchString(c.Repository) {
		return CheckedOut{}, errors.New("checkout: the room or its repository is malformed")
	}
	git := c.Git
	if git == nil {
		git = RunGit
	}
	branch := "agent/" + c.Room
	out := CheckedOut{Branch: branch}
	if c.PR > 0 {
		out.PR = "https://github.com/" + c.Repository + "/pull/" + strconv.Itoa(c.PR)
	}
	clone := refuse(OneLine(c.Workspace)+" is not a checkout of "+c.Repository, "gh repo clone "+c.Repository)
	top, err := git(ctx, c.Workspace, "rev-parse", "--show-toplevel")
	if err != nil {
		return out, clone
	}
	top = strings.TrimRight(top, "\n")
	origin, err := git(ctx, top, "config", "--get", "remote.origin.url")
	if err != nil || !originRE(c.Repository).MatchString(strings.TrimSpace(origin)) {
		return out, clone
	}
	remote := "refs/heads/" + branch
	heads, err := git(ctx, top, "ls-remote", "--heads", "origin", remote)
	if err != nil {
		return out, fmt.Errorf("checkout: asking origin for %s: %w", branch, err)
	}
	if strings.TrimSpace(heads) == "" {
		why := branch + " is gone from origin"
		if out.PR != "" {
			return out, refuse(why+": the pull request was merged or closed, see "+out.PR+"; a merged change is on the default branch", "")
		}
		return out, refuse(why+", and the room opened no pull request", "")
	}
	if path, err := checkedOutAt(ctx, git, top, remote); err != nil {
		return out, err
	} else if path != "" {
		out.Path, out.Reused = path, true
		return out, nil
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(top))
	if err != nil {
		return out, fmt.Errorf("checkout: %w", err)
	}
	path := filepath.Join(parent, filepath.Base(top)+"."+c.Room)
	if filepath.Dir(path) != parent || strings.Contains(filepath.Base(path), "..") {
		return out, errors.New("checkout: the worktree path leaves the repository's directory")
	}
	if _, err := os.Lstat(path); err == nil {
		return out, refuse(OneLine(path)+" already exists and is not a worktree of "+branch, "move it away, or check "+branch+" out yourself")
	} else if !errors.Is(err, os.ErrNotExist) {
		return out, fmt.Errorf("checkout: %w", err)
	}
	if _, err := git(ctx, top, "fetch", "--no-tags", "origin", "+"+remote+":refs/remotes/origin/"+branch); err != nil {
		return out, fmt.Errorf("checkout: fetching %s: %w", branch, err)
	}
	remoteRef, localRef := "refs/remotes/origin/"+branch, remote
	_, noLocal := git(ctx, top, "show-ref", "--verify", "--quiet", localRef)
	refs := []string{remoteRef}
	if noLocal == nil { // the worktree gets the local branch: it is checked too
		refs = append(refs, localRef)
	}
	files, err := c.flaggedFiles(ctx, git, top, refs)
	if err != nil {
		return out, err
	}
	if len(files) > 0 {
		shown := capNames(files)
		if c.Confirm == nil {
			return out, refuse(branch+" changes files that run code on your machine, and this client cannot ask you: "+strings.Join(shown, ", "),
				"read them in the pull request, then check the branch out yourself")
		}
		ok, err := c.Confirm(ctx, shown)
		if err != nil {
			return out, refuse("the developer was not asked: "+OneLine(err.Error()), "")
		}
		if !ok {
			return out, refuse("not created: the developer did not confirm the files that run code ("+strings.Join(shown, ", ")+")", "")
		}
	}
	args := []string{"worktree", "add", "--track", "-b", branch, path, remoteRef}
	if noLocal == nil {
		args = []string{"worktree", "add", path, branch}
	}
	if _, err := git(ctx, top, args...); err != nil {
		return out, fmt.Errorf("checkout: %w", err)
	}
	out.Path = path
	return out, nil
}

// checkedOutAt is the path of the worktree that has ref checked out, "" when none has.
func checkedOutAt(ctx context.Context, git Git, top, ref string) (string, error) {
	list, err := git(ctx, top, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return "", fmt.Errorf("checkout: %w", err)
	}
	var path string
	for _, f := range strings.Split(list, "\x00") {
		switch {
		case strings.HasPrefix(f, "worktree "):
			path = strings.TrimPrefix(f, "worktree ")
		case f == "branch "+ref:
			return path, nil
		}
	}
	return "", nil
}

// flaggedFiles are the files each ref changes against its merge base with HEAD (R15) that match
// flagged or the repository's in-tree hooks path: all its files when it shares no history.
func (c Checkout) flaggedFiles(ctx context.Context, git Git, top string, refs []string) ([]string, error) {
	prefixes := slices.Clone(flagged)
	if hp, err := git(ctx, top, "config", "--local", "--get", "core.hooksPath"); err == nil {
		if p := inTreeHooks(top, strings.TrimSpace(hp)); p != "" {
			prefixes = append(prefixes, p)
		}
	}
	var all []string
	for _, ref := range refs {
		var names string
		var err error
		if base, mbErr := git(ctx, top, "merge-base", "HEAD", ref); mbErr == nil {
			names, err = git(ctx, top, "diff", "--name-only", "--no-renames", "-z", strings.TrimSpace(base), ref)
		} else {
			names, err = git(ctx, top, "ls-tree", "-r", "--name-only", "-z", ref)
		}
		if err != nil {
			return nil, fmt.Errorf("checkout: listing the branch's changes: %w", err)
		}
		for _, f := range strings.Split(names, "\x00") {
			if f != "" && isFlagged(f, prefixes) {
				all = append(all, OneLine(f))
			}
		}
	}
	slices.Sort(all)
	return slices.Compact(all), nil
}

// inTreeHooks is core.hooksPath as a prefix of the repository's files, "" when it points outside.
func inTreeHooks(top, hp string) string {
	if hp == "" {
		return ""
	}
	if filepath.IsAbs(hp) {
		rel, err := filepath.Rel(top, hp)
		if err != nil {
			return ""
		}
		hp = rel
	}
	hp = filepath.ToSlash(filepath.Clean(hp))
	switch {
	case hp == "..", strings.HasPrefix(hp, "../"):
		return ""
	case hp == ".":
		return "/" // every file is a hook
	}
	return hp + "/"
}

// isFlagged is whether file is under a flagged entry: at the root, or in a subdirectory (a tool
// started there reads its own folder), in any case. The hooks path entry "/" matches every file.
func isFlagged(file string, entries []string) bool {
	lf := strings.ToLower(file)
	for _, e := range entries {
		le := strings.ToLower(e)
		switch {
		case le == "/":
			return true
		case strings.HasSuffix(le, "/"):
			if strings.HasPrefix(lf, le) || strings.Contains(lf, "/"+le) {
				return true
			}
		case lf == le || strings.HasSuffix(lf, "/"+le):
			return true
		}
	}
	return false
}

// capNames keeps the first maxConfirmed names and counts the rest.
func capNames(files []string) []string {
	if len(files) <= maxConfirmed {
		return files
	}
	return append(slices.Clone(files[:maxConfirmed]), fmt.Sprintf("and %d more", len(files)-maxConfirmed))
}

// room_checkout's tools/list entry.
var roomCheckoutEntry = map[string]any{
	"name": toolRoomCheckout,
	"description": "Check out the branch a room's agent worked on, agent/<room>, into a new git worktree beside the developer's " +
		"repository, and return its path and pull request. It never touches the current checkout, so a dirty tree or the " +
		"session's branch is safe, and a second call returns the same worktree. The workspace must be a checkout of the room's " +
		"repository; otherwise it refuses and gives the gh repo clone command. When the branch changes files that run code on " +
		"open (agent or editor config, hooks, mise), it asks the developer in a form, and refuses where the client cannot show one. " +
		"It runs nothing in the worktree.",
	"inputSchema": json.RawMessage(`{"type":"object","additionalProperties":false,"required":["room"],"properties":{` +
		`"room":{"type":"string","pattern":"^[a-z2-7]{8}$","description":"the room's id, from rooms_list"}}}`),
}

// roomRow is the room's row, which gives its repository and pull request: the open rooms first,
// then the ones that ended over a week ago.
func (r *Relay) roomRow(ctx context.Context, room string) (roomview.Row, bool) {
	for _, args := range []map[string]any{{}, {"all": true}} {
		rows, err := r.listRows(ctx, args)
		if err != nil {
			return roomview.Row{}, false
		}
		for _, row := range rows {
			if row.ID == room {
				return row, true
			}
		}
	}
	return roomview.Row{}, false
}

// roomCheckout is room_checkout. The repository and the pull request come from the broker's row
// of the room, never from the agent's text; the developer's yes comes from a form.
func (r *Relay) roomCheckout(ctx context.Context, c localCall) any {
	var a struct {
		Room string `json:"room"`
	}
	if mcpwire.DecodeArgs(c.Args, &a) != nil || !roomIDRE.MatchString(a.Room) {
		return mcpwire.ToolError("invalid_arguments: room is a room id")
	}
	row, ok := r.roomRow(ctx, a.Room)
	if !ok {
		return mcpwire.ToolError("unknown_room: " + a.Room + " is not one of the rooms you can see")
	}
	dir, err := r.workspace(ctx)
	if err != nil {
		return mcpwire.ToolError("not_checked_out: " + OneLine(err.Error()))
	}
	r.mu.Lock()
	forms := r.forms
	r.mu.Unlock()
	co := Checkout{Git: r.Git, Workspace: dir, Room: a.Room, Repository: row.Repository, PR: row.PR}
	if forms {
		co.Confirm = func(ctx context.Context, files []string) (bool, error) {
			return r.confirmCheckout(ctx, row.Repository, a.Room, files)
		}
	}
	got, err := co.Run(ctx)
	if err != nil {
		var ref *CheckoutRefusal
		if errors.As(err, &ref) {
			return mcpwire.ToolError("refused: " + OneLine(ref.Error()))
		}
		return mcpwire.ToolError("not_checked_out: " + OneLine(err.Error()))
	}
	out, _ := mcpwire.ToolResult(got)
	return out
}

// confirmCheckout asks the developer, in a form, whether to create a worktree that holds files
// which run code. The answer defaults to no.
func (r *Relay) confirmCheckout(ctx context.Context, repo, room string, files []string) (bool, error) {
	ans, err := r.ask(ctx, "elicitation/create", map[string]any{
		"message": fmt.Sprintf("Check out agent/%s of %s into a new worktree? The branch changes files that run code on your machine "+
			"when a tool opens that folder or you commit there. Nothing runs now.\n\n%s", room, repo, "- "+strings.Join(files, "\n- ")),
		"requestedSchema": map[string]any{"type": "object", "required": []string{"create"}, "properties": map[string]any{
			"create": map[string]any{"type": "boolean", "title": "Create the worktree", "default": false},
		}},
	})
	if err != nil {
		return false, err
	}
	var res struct {
		Action  string `json:"action"`
		Content struct {
			Create bool `json:"create"`
		} `json:"content"`
	}
	if len(ans.Error) > 0 || json.Unmarshal(ans.Result, &res) != nil {
		return false, errors.New("the client could not ask the developer")
	}
	return res.Action == "accept" && res.Content.Create, nil
}
