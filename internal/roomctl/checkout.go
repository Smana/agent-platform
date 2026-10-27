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
// snippet and skill folder roomctl setup writes. Flagging more is safe: a flag only means "ask first". A changed
// symlink is flagged whatever its name, and so is a changed path that is a directory of an entry,
// since a link or a file there redirects what the entry's tools read.
var flagged = []string{
	// Agent and editor configuration, whole: hooks, tools, plugins, skills and scripts all live beside the settings.
	".claude/", ".cursor/", ".opencode/", ".gemini/", ".codex/", ".vscode/", ".agents/", ".devcontainer/",
	".github/skills/", ".github/mcp.json",
	".mcp.json", "opencode.json", "opencode.jsonc",
	".envrc", "mise.toml", ".mise.toml", "mise.local.toml",
	".gitmodules", ".lfsconfig",
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
	origin, err := git(ctx, top, "config", "--get-all", "remote.origin.url")
	urls := strings.Fields(origin)
	if err != nil || len(urls) == 0 {
		return out, clone
	}
	if len(urls) > 1 { // fetch uses the first, config --get the last: refuse the ambiguity
		return out, refuse("origin of "+OneLine(c.Workspace)+" has more than one URL", "gh repo clone "+c.Repository)
	}
	if !originRE(c.Repository).MatchString(urls[0]) {
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
	path, err := reusable(ctx, git, top, remote)
	if err != nil {
		return out, err
	}
	if path != "" {
		out.Path, out.Reused = path, true
		return out, nil
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(top))
	if err != nil {
		return out, fmt.Errorf("checkout: %w", err)
	}
	path = filepath.Join(parent, filepath.Base(top)+"."+c.Room)
	if filepath.Dir(path) != parent || strings.Contains(filepath.Base(path), "..") {
		return out, errors.New("checkout: the worktree path leaves the repository's directory")
	}
	if err := freePath(path, branch); err != nil {
		return out, err
	}
	remoteRef := "refs/remotes/origin/" + branch
	if _, err := git(ctx, top, "fetch", "--no-tags", "origin", "+"+remote+":"+remoteRef); err != nil {
		return out, fmt.Errorf("checkout: fetching %s: %w", branch, err)
	}
	// From here on only this commit is checked, confirmed and created: the ref can move under a
	// concurrent fetch, the commit cannot.
	sha, err := revParse(ctx, git, top, remoteRef)
	if err != nil {
		return out, fmt.Errorf("checkout: %w", err)
	}
	local, err := revParse(ctx, git, top, remote)
	haveLocal := err == nil
	if haveLocal && local != sha {
		return out, refuse("a local branch "+branch+" exists and is not the commit origin has ("+local[:12]+", not "+sha[:12]+")",
			"git branch -D "+branch+" to drop it, or git branch -f "+branch+" "+sha[:12]+" to move it")
	}
	files, err := c.flaggedFiles(ctx, git, top, sha)
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
	if err := freePath(path, branch); err != nil { // the form can take minutes
		return out, err
	}
	args := []string{"worktree", "add", "-b", branch, path, sha}
	if haveLocal {
		args = []string{"worktree", "add", path, branch}
	}
	// Whatever ends badly from here (a cancellation kills git half way) leaves nothing behind.
	undo := func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), gitTimeout)
		defer cancel()
		_, _ = git(cctx, top, "worktree", "remove", "--force", "--force", path)
		_, _ = git(cctx, top, "worktree", "prune")
		if !haveLocal {
			_, _ = git(cctx, top, "branch", "-D", branch)
		}
	}
	if _, err := git(ctx, top, args...); err != nil {
		undo()
		return out, fmt.Errorf("checkout: %w", err)
	}
	if head, err := revParse(ctx, git, path, "HEAD"); err != nil || head != sha {
		undo()
		return out, fmt.Errorf("checkout: the new worktree is not the commit that was checked (%s): it was removed", OneLine(sha[:12]))
	}
	if resolved, err := filepath.EvalSymlinks(path); err != nil || resolved != path || filepath.Dir(resolved) != parent {
		return out, fmt.Errorf("checkout: %s is not where the worktree was meant to be: check it before you use it", OneLine(path))
	}
	if !haveLocal { // best effort: the branch follows origin's
		_, _ = git(ctx, top, "branch", "--set-upstream-to=origin/"+branch, branch)
	}
	out.Path = path
	return out, nil
}

// revParse is the full id of the commit ref names.
func revParse(ctx context.Context, git Git, dir, ref string) (string, error) {
	out, err := git(ctx, dir, "rev-parse", "--verify", "--quiet", ref+"^{commit}")
	sha := strings.TrimSpace(out)
	if err != nil || !shaRE.MatchString(sha) {
		return "", fmt.Errorf("%s is not a commit", ref)
	}
	return sha, nil
}

var shaRE = regexp.MustCompile(`^[0-9a-f]{40,64}$`)

// freePath refuses a path that exists, a link included: nothing is overwritten or written through.
func freePath(path, branch string) error {
	_, err := os.Lstat(path)
	switch {
	case err == nil:
		return refuse(OneLine(path)+" already exists and is not a worktree of "+branch, "move it away, or check "+branch+" out yourself")
	case !errors.Is(err, os.ErrNotExist):
		return fmt.Errorf("checkout: %w", err)
	}
	return nil
}

// reusable is the path of the worktree that has ref checked out and can be used as it is: "" when
// there is none. One whose folder is gone, or that git left locked as "initializing" (a checkout
// that was killed half way), is removed first, and made again by the caller.
func reusable(ctx context.Context, git Git, top, ref string) (string, error) {
	for range 2 {
		path, reason, locked, err := checkedOutAt(ctx, git, top, ref)
		if err != nil || path == "" {
			return "", err
		}
		_, statErr := os.Stat(path)
		switch {
		case statErr == nil && (!locked || reason != "initializing"):
			return path, nil
		case locked && reason == "initializing":
			if _, err := git(ctx, top, "worktree", "remove", "--force", "--force", path); err != nil {
				return "", refuse(OneLine(path)+" is a half-made worktree of "+ref+" that cannot be removed: "+OneLine(err.Error()), "git worktree remove --force --force <path>")
			}
		}
		if _, err := git(ctx, top, "worktree", "prune"); err != nil {
			return "", fmt.Errorf("checkout: %w", err)
		}
	}
	return "", refuse("a worktree of "+ref+" is left over and cannot be removed", "git worktree prune")
}

// checkedOutAt is the worktree that has ref checked out: its path, and whether and why it is locked.
func checkedOutAt(ctx context.Context, git Git, top, ref string) (path, reason string, locked bool, err error) {
	list, err := git(ctx, top, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return "", "", false, fmt.Errorf("checkout: %w", err)
	}
	var cur, curReason string
	var curLocked bool
	for _, f := range append(strings.Split(list, "\x00"), "") { // an empty field ends a record
		switch {
		case strings.HasPrefix(f, "worktree "):
			cur, curReason, curLocked = strings.TrimPrefix(f, "worktree "), "", false
			path = ""
		case f == "branch "+ref:
			path = cur
		case f == "locked" || strings.HasPrefix(f, "locked "):
			curLocked, curReason = true, strings.TrimPrefix(strings.TrimPrefix(f, "locked"), " ")
		case f == "":
			if path != "" && path == cur {
				return path, curReason, curLocked, nil
			}
		}
	}
	return "", "", false, nil
}

// symlinkMode is git's mode for a symbolic link.
const symlinkMode = "120000"

// flaggedFiles are what the commit changes that is flagged (R15), against its merge base with HEAD
// and against HEAD itself (a flagged file the branch carries unchanged, which HEAD has since fixed,
// differs from HEAD): a file that matches the entries or the repository's in-tree hooks path, a
// changed path that is a directory of one, any changed symlink, and any path with a non-ASCII byte.
// A symlink the commit has at a flagged place is flagged too, since the branch can change what it
// points to. With no shared history it is all of the commit's files.
func (c Checkout) flaggedFiles(ctx context.Context, git Git, top, sha string) ([]string, error) {
	entries := slices.Clone(flagged)
	if hp, err := git(ctx, top, "config", "--show-scope", "--get-all", "core.hooksPath"); err == nil {
		if p := inTreeHooks(top, effectiveValue(hp)); p != "" {
			entries = append(entries, p)
		}
	}
	var all []string
	add := func(mode, file string) {
		if file != "" && (mode == symlinkMode || isFlagged(file, entries)) {
			all = append(all, OneLine(file))
		}
	}
	tree, err := git(ctx, top, "ls-tree", "-r", "-z", sha)
	if err != nil {
		return nil, fmt.Errorf("checkout: listing the branch's files: %w", err)
	}
	base, mbErr := git(ctx, top, "merge-base", "HEAD", sha)
	for _, rec := range strings.Split(tree, "\x00") {
		mode, file := lsTreeEntry(rec)
		if mode == symlinkMode && isFlagged(file, entries) || mbErr != nil { // no shared history: every file
			add(mode, file)
		}
	}
	diff := func(from string) error {
		raw, err := git(ctx, top, "diff", "--raw", "--no-abbrev", "--no-renames", "-z", from, sha)
		if err != nil {
			return err
		}
		fields := strings.Split(raw, "\x00")
		for i := 0; i+1 < len(fields); i += 2 { // ":<old mode> <new mode> <old> <new> <status>", then the path
			meta := strings.Fields(strings.TrimPrefix(fields[i], ":"))
			if len(meta) < 2 {
				return errors.New("git diff printed something unexpected")
			}
			mode := ""
			if slices.Contains(meta[:2], symlinkMode) {
				mode = symlinkMode
			}
			add(mode, fields[i+1])
		}
		return nil
	}
	if mbErr == nil {
		if err := diff(strings.TrimSpace(base)); err != nil {
			return nil, fmt.Errorf("checkout: listing the branch's changes: %w", err)
		}
	}
	if err := diff("HEAD"); err != nil && mbErr == nil && !strings.Contains(err.Error(), "HEAD") {
		return nil, fmt.Errorf("checkout: listing the branch's changes: %w", err)
	} // an unborn HEAD has nothing to differ from
	slices.Sort(all)
	return slices.Compact(all), nil
}

// lsTreeEntry is the mode and the path of one "<mode> <type> <sha>\t<path>" record.
func lsTreeEntry(rec string) (mode, path string) {
	meta, path, ok := strings.Cut(rec, "\t")
	if !ok {
		return "", ""
	}
	mode, _, _ = strings.Cut(meta, " ")
	return mode, path
}

// effectiveValue is the last value of `git config --show-scope --get-all` that is not the
// command line's: RunGit sets core.hooksPath there, over the repository's own.
func effectiveValue(out string) string {
	val := ""
	for _, line := range strings.Split(out, "\n") {
		if scope, v, ok := strings.Cut(line, "\t"); ok && scope != "command" {
			val = strings.TrimSpace(v)
		}
	}
	return val
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

// dirNames are the entries without their trailing "/" and each directory above them: the names
// at which a link or a file redirects what an entry's tools read. The hooks entry "/" has none.
func dirNames(entries []string) []string {
	var names []string
	for _, e := range entries {
		if e == "/" {
			continue
		}
		parts := strings.Split(strings.TrimSuffix(e, "/"), "/")
		for i := range parts {
			names = append(names, strings.Join(parts[:i+1], "/"))
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

// isFlagged is whether file is under a flagged entry, or is a directory of one: at the root or in
// a subdirectory (a tool started there reads its own folder), in ASCII case, or has a byte past
// ASCII in its name. The hooks path entry "/" matches every file.
func isFlagged(file string, entries []string) bool {
	for i := 0; i < len(file); i++ {
		if file[i] >= 0x80 { // a filesystem may fold it into an ASCII name: never chase the foldings
			return true
		}
	}
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
	for _, n := range dirNames(entries) {
		if ln := strings.ToLower(n); lf == ln || strings.HasSuffix(lf, "/"+ln) {
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
// A broker that cannot answer is an error, not a miss; the second list is best effort, since an
// older broker may not know "all".
func (r *Relay) roomRow(ctx context.Context, room string) (roomview.Row, bool, error) {
	for i, args := range []map[string]any{{}, {"all": true}} {
		rows, err := r.listRows(ctx, args)
		if err != nil {
			if i == 0 {
				return roomview.Row{}, false, err
			}
			break
		}
		for _, row := range rows {
			if row.ID == room {
				return row, true, nil
			}
		}
	}
	return roomview.Row{}, false, nil
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
	row, ok, err := r.roomRow(ctx, a.Room)
	if err != nil {
		return mcpwire.ToolError("not_checked_out: " + OneLine(explainRelay(err)))
	}
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

// CodeSpans is the names in code spans; the "and N more" line is ours, not a name.
func CodeSpans(files []string) []string {
	out := make([]string, len(files))
	for i, f := range files {
		if strings.HasPrefix(f, "and ") && strings.HasSuffix(f, " more") && i == len(files)-1 && len(files) == maxConfirmed+1 {
			out[i] = f
			continue
		}
		out[i] = CodeSpan(f)
	}
	return out
}

// confirmCheckout asks the developer, in a form, whether to create a worktree that holds files
// which run code. The answer defaults to no.
func (r *Relay) confirmCheckout(ctx context.Context, repo, room string, files []string) (bool, error) {
	ans, err := r.ask(ctx, "elicitation/create", map[string]any{
		"message": fmt.Sprintf("Check out agent/%s of %s into a new worktree? The branch changes files that run code on your machine "+
			"when a tool opens that folder or you commit there. Nothing runs now.\n\n%s", room, repo, "- "+strings.Join(CodeSpans(files), "\n- ")),
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

// CodeSpan is a file name as the developer sees it: in a code span, so that nothing in it renders
// as a link or emphasis, with the backticks that would end the span replaced, and cut at 120 runes.
func CodeSpan(name string) string {
	r := []rune(strings.ReplaceAll(name, "`", "'"))
	if len(r) > 120 {
		r = append(r[:117], '.', '.', '.')
	}
	return "`" + string(r) + "`"
}
