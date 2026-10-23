// SPDX-License-Identifier: Apache-2.0

// Package forge is the factory's view of GitHub, through the factory's own App (C6): one
// repository per instance (OD-6). GitHub is the adapter, forgetest holds the test double for the packages that
// consume it; each consumer declares the interface it needs (AGENTS.md) — Merger is the one
// exception the owner ruled (2026-09-27; R16), because its key must reach only the factory, and a
// seam defined here keeps it out of every consumer's hands. Logins are REST-shaped: a bot is
// always "name[bot]", from REST or from GraphQL.
package forge

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ErrEventsTruncated is LabelEvents reaching its page cap: the newest events are unread.
var ErrEventsTruncated = errors.New("forge: label events past the page cap are unread")

// ErrCommitsTruncated is PullRequestCommits reaching GitHub's 250-commit cap: commits past it are
// never listed, so the list read is not known to be the whole pull request.
var ErrCommitsTruncated = errors.New("forge: a pull request's commits past GitHub's 250 are unread")

// ErrDiffTooLarge is Diff reaching a full page of files: the files past it are unseen, so the
// diff cannot be judged. The merge path hands such a pull request to a human (patch_too_large).
var ErrDiffTooLarge = errors.New("forge: a diff of a full page of files, the rest unread")

// ErrActivityTruncated is BranchActivity reaching its page cap before the branch's creation: the
// pushes before what was read are unknown.
var ErrActivityTruncated = errors.New("forge: a branch's activity past the page cap is unread")

// ErrHeadMoved and ErrNotMergeable are GitHub's refusals of a merge at merge time (ruling R52):
// expectedHeadOid is no longer the head — a push landed after the decision (HTTP 409) — or the
// pull request cannot be merged as it stands: protections, an approval it lacks (HTTP 405).
// Both are decisions the factory can make, so they are typed; anything else is a plain error.
var (
	ErrHeadMoved    = errors.New("forge: the pull request's head moved since the decision")
	ErrNotMergeable = errors.New("forge: GitHub does not allow the merge as it stands")
)

// ErrNotAFile is DefaultBranchFile at a path that is never a readable file: a directory, a file
// over the contents API's 1 MB, a symlink or a submodule. It does not heal by retrying.
var ErrNotAFile = errors.New("forge: the path is not a readable file")

// ErrMergedTruncated is MergedSince filling its window of 100: pull requests past it are unread,
// so the caller narrows since and reads again.
var ErrMergedTruncated = errors.New("forge: a window of 100 merged pull requests, the rest unread")

// Item is an open issue or pull request carrying a label.
type Item struct {
	Number      int
	PullRequest bool
}

// AgentPull is one open pull request from an `agent/` branch, as the orphan scan reads it (R51):
// the head branch, its labels, and whether that branch is this repository's own — a fork's never
// is, so its pull requests are never the factory's to judge.
type AgentPull struct {
	Number  int
	HeadRef string
	Labels  []string
	Fork    bool
}

// LabelEvent is one time a label was added, and by whom (R4: only maintainers' count).
type LabelEvent struct {
	Actor string
	Label string
	At    time.Time
}

// Comment is an issue or pull request comment. Edited: its body changed after it was posted,
// perhaps by someone other than its author, so a command in it is never acted on.
type Comment struct {
	ID     int64
	Author string
	Body   string
	At     time.Time
	Edited bool
}

// Issue is the snapshot's source (§1, R5).
type Issue struct {
	Number int
	// ID is REST's numeric id, which the sub-issues API takes (splitter SL8); 0 when read through
	// GraphQL (Issue).
	ID           int64
	URL          string
	Title        string
	Body         string
	State        string // OPEN | CLOSED
	Author       string // a bot's with "[bot]", as REST writes it
	Labels       []string
	LastEditedAt time.Time // the body's last edit; zero when never edited
	// The last title rename (a RenamedTitleEvent): lastEditedAt does not cover the title,
	// which is part of the snapshot (R5).
	TitleEditedAt time.Time
}

// ReviewComment is a review's comment on one line of the diff.
type ReviewComment struct {
	Path string
	Line int
	Body string
}

// Review is a pull request review.
type Review struct {
	ID       int64
	Author   string
	State    string // APPROVED | CHANGES_REQUESTED | COMMENTED | DISMISSED
	Body     string
	At       time.Time
	Comments []ReviewComment
}

// PR is one GraphQL snapshot of a pull request. Checks and statuses are a separate query on the
// Merger: reading them needs checks and statuses read, which only the merger App holds (R16).
type PR struct {
	Number      int
	NodeID      string
	URL         string
	Title       string
	Body        string // the description: a claim by whoever wrote it
	State       string // OPEN | CLOSED | MERGED
	Author      string
	HeadRef     string
	HeadSHA     string
	HeadMessage string
	// The head commit's committer date: set by whoever committed, so a claim like Trailer's.
	HeadCommittedAt time.Time
	MergedBy        string
	MergedAt        time.Time // zero while unmerged
	MergeCommitSHA  string
	AutoMerge       bool
	Labels          []string
	Reviews         []Review
	Comments        []Comment
}

// Trailer is what the head commit claims for a git trailer, "" when the exact key is absent; the
// last occurrence wins. Only the message's last paragraph holds trailers, as git reads them, and
// never its subject.
//
// It is a claim, never authentication (review B1). The harness's commit-msg hook marks every
// other Agent-* line, in any case, as the model's, then appends the real ones with
// `git interpret-trailers --if-exists add`, so the hook's own is the last occurrence. But an agent
// with a shell can skip or rewrite the hook, and anyone can copy a run's trailer: run IDs are
// public, and an amend or a cherry-pick keeps it (#76). Callers must fail closed on an absent,
// foreign, duplicated or case-variant Agent-Run, and a matching one proves only that the commit
// claims to be the run's.
func (p PR) Trailer(key string) string { return trailer(p.HeadMessage, key) }

// Commit is one commit of a pull request, as GitHub lists them.
type Commit struct {
	SHA     string
	Author  string // the login GitHub matched the author's email to, "" when none: never proof
	Message string
}

// Push is one move of a branch, as GitHub's repository activity records it (#76): Actor is the
// account whose credential moved the ref from Before to After, "" when GitHub names none. Unlike
// a commit's author or trailer, the pusher is GitHub's record, not the pusher's claim.
type Push struct {
	Type   string // push | force_push | branch_creation | branch_deletion | pr_merge | merge_queue_merge
	Actor  string
	Before string
	After  string
	At     time.Time
}

// Trailer is what the commit claims for a git trailer, as PR.Trailer reads the head's.
func (c Commit) Trailer(key string) string { return trailer(c.Message, key) }

func trailer(message, key string) string {
	msg := strings.TrimRight(strings.ReplaceAll(message, "\r\n", "\n"), " \t\n")
	i := strings.LastIndex(msg, "\n\n")
	if i < 0 {
		return ""
	}
	v := ""
	for _, line := range strings.Split(msg[i+2:], "\n") {
		if rest, ok := strings.CutPrefix(line, key+":"); ok {
			v = strings.TrimSpace(rest)
		}
	}
	return v
}

// Check is one CI check run on a commit: SUCCESS, FAILURE, CANCELLED or PENDING. CANCELLED is
// apart from FAILURE because nothing failed: the runner or a concurrency group stopped the job.
type Check struct {
	ID    int64 // the check run's database id, which is its Actions job id
	Name  string
	State string // SUCCESS | FAILURE | CANCELLED | PENDING
	// App is the database id of the GitHub App whose check suite holds the run: GitHub Actions
	// is ActionsApp. 0 when GitHub did not say, and always 0 on the checks CommitChecks reads (main's
	// CI), which carry no App.
	App int64
	// Conclusion is the check run's raw conclusion (SUCCESS, NEUTRAL, SKIPPED, FAILURE…), "" while
	// it runs. State folds NEUTRAL and SKIPPED into SUCCESS, as classic protection does; a gate
	// that needs the job to have run reads this.
	Conclusion string
}

// ActionsApp is GitHub Actions' App id on github.com, the one classic protection pins required
// checks to (15368).
const ActionsApp int64 = 15368

// FileStat is one changed file as GitHub's compare lists it: its status (added, removed,
// modified, renamed, copied, changed, unchanged), its previous name on a rename, and its line
// counts, which policy-bot's modified_lines sums.
type FileStat struct {
	Path      string
	Previous  string
	Status    string
	Additions int
	Deletions int
}

// Diff is what a squash merge of head applies (R52): each changed file's content at the merge
// base and at head, "" where it is absent, and GitHub's own list of the changed files.
type Diff struct {
	Base, Head map[string]string
	Files      []FileStat
}

// Status is one classic commit status, policy-bot's among them.
type Status struct {
	Context string
	State   string
	Creator string
}

// Checks is a head commit's rollup: check runs (CI) and commit statuses (policy-bot).
type Checks struct {
	Runs     []Check
	Statuses []Status
	// Truncated: the rollup has more contexts than the one page read, so a check may be unseen.
	Truncated bool
}

// StatusOf is the state and creator of the named commit status; "", "" when it is absent.
func (c Checks) StatusOf(context string) (state, creator string) {
	for _, s := range c.Statuses {
		if s.Context == context {
			return s.State, s.Creator
		}
	}
	return "", ""
}

// WorkflowRun is one completed run of a workflow, as the dead-links probe reads it: what
// triggered it, on which branch, by whom, and how it ended.
type WorkflowRun struct {
	ID         int64
	RunAttempt int    // 1, then one more per re-run of the same run ID
	Event      string // schedule | workflow_dispatch | pull_request | push | …
	Branch     string // the run's head branch
	Actor      string // the login that triggered it
	Conclusion string // success | failure | cancelled | …
	Created    time.Time
}

// Revert is the pull request RevertPR opened.
type Revert struct {
	Number int
	URL    string
	NodeID string
}

// LabelledPull is one pull request carrying a label, as the merge breaker's ledger reads it
// (v2 D1): its state and, once merged, when.
type LabelledPull struct {
	Number   int
	State    string // OPEN | CLOSED | MERGED
	MergedAt time.Time
}

// MergedPull is one merged pull request, as the merge monitor's follow-up scan reads it: who
// authored it, from which branch, the commit it became on the default branch, when, and its
// labels.
type MergedPull struct {
	Number         int
	Author         string // REST-shaped: a bot's with "[bot]"
	HeadRef        string
	MergeCommitSHA string
	Title          string
	MergedAt       time.Time
	Labels         []string
	// Commits is how many commits the pull request had; ParentPulls the pull requests its merge
	// commit's first parent belongs to. Together they tell a rebase merge of several commits (the
	// first parent is then one of its own commits, not the base) from a squash.
	Commits     int
	ParentPulls []int
}

// MultiCommitRebase reports a pull request that may have been merged by rebasing more than one
// commit: its merge commit's first parent is a commit of the same pull request, or GitHub has not
// yet said which pull requests the parent belongs to (a lagging answer must not read as a squash). A single-commit rebase has the base
// for parent, as a squash does.
func (p MergedPull) MultiCommitRebase() bool {
	return p.Commits > 1 && (len(p.ParentPulls) == 0 || slices.Contains(p.ParentPulls, p.Number))
}

// FilePatch is one file a pull request changed: its path, its previous path on a rename, GitHub's
// status for it and its unified patch ("" for a binary, a rename alone or a patch GitHub omits).
type FilePatch struct {
	Path     string
	Previous string
	Status   string
	Patch    string
	// PatchMissing: GitHub omitted the patch although the file changed (a large or binary diff),
	// so BaseLines is unknown.
	PatchMissing bool
}

// BlameRange is a run of lines, Start to End inclusive, last changed by Commit.
type BlameRange struct {
	Start, End int
	Commit     string
}

// ErrNoPatch is the base lines of a file whose patch GitHub omitted (a large or binary diff): which
// lines changed is unknown, never "none".
var ErrNoPatch = errors.New("forge: GitHub omitted the file's patch, the lines it changed are unknown")

// ErrNoBlame is BlameAtParent finding nothing to blame: the path is not in the parent, or GitHub
// returned no blame for it. It is not "no overlap": the caller treats it as inconclusive.
var ErrNoBlame = errors.New("forge: nothing to blame at the parent for the path")

var hunkRE = regexp.MustCompile(`^@@ -(\d+)(?:,(\d+))? \+\d+(?:,\d+)? @@`)

// BaseLines are the base-side line numbers a unified patch removes or rewrites: the lines of the
// file as it was that the pull request changed (merge gate R35). A pure addition changes none.
// The result is sorted and deduplicated, nil when there is none. A hunk header that does not parse
// is an error: dropping the hunk would read as "no change".
func BaseLines(patch string) ([]int, error) {
	var out []int
	base := 0
	for line := range strings.SplitSeq(patch, "\n") {
		if strings.HasPrefix(line, "@@") {
			m := hunkRE.FindStringSubmatch(line)
			if m == nil {
				return nil, fmt.Errorf("forge: malformed hunk header %q", line)
			}
			base, _ = strconv.Atoi(m[1])
			if count := m[2]; base == 0 && count != "0" { // only a new file starts at line 0
				return nil, fmt.Errorf("forge: malformed hunk header %q", line)
			}
			continue
		}
		if line == "" { // a context line stripped of its leading space
			base++
			continue
		}
		switch line[0] {
		case ' ':
			base++
		case '-':
			out = append(out, base)
			base++
		}
	}
	if out == nil {
		return nil, nil
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

// BaseLines is BaseLines of the file's patch, or ErrNoPatch when GitHub omitted it.
func (f FilePatch) BaseLines() ([]int, error) {
	if f.PatchMissing {
		return nil, ErrNoPatch
	}
	return BaseLines(f.Patch)
}

// BlamePath is the name to blame the file under at the parent: its previous name on a rename.
func (f FilePatch) BlamePath() string {
	if f.Previous != "" {
		return f.Previous
	}
	return f.Path
}

// PRSummary is one open pull request: enough to find the live ones without reading each in full.
type PRSummary struct {
	Number  int
	Author  string
	Created time.Time
}

// Merger is GitHub through the merger App (owner, 2026-09-27; R16): the only identity that reads
// checks, statuses and the changed files of a decision, merges on a decision and opens and arms
// reverts. Its key lives in the factory alone; the factory App, whose key the broker shares,
// never gets these powers.
//
// Merge replaces arming auto-merge for the factory's decision (external review R02, ruling R52):
// GitHub checks the head at merge time here, while auto-merge's expectedHeadOid is only an
// enable-time check and a disarm-by-polling path fails open while the factory is down.
// EnableAutoMerge stays for one use — the revert PR of §6.4 — where waiting for green and
// merging without the factory is the point: main is red, the branch is the merger's own, and
// no push can move it after the arm.
type Merger interface {
	PullRequestChecks(ctx context.Context, number int) (Checks, error)
	CommitChecks(ctx context.Context, sha string) ([]Check, error)
	// IsAncestor reports whether ancestor is sha or one of its ancestors: the monitor asks it
	// whether a later main commit carries a merge.
	IsAncestor(ctx context.Context, ancestor, sha string) (bool, error)
	// RerunCheck re-runs the Actions job a check run is. It needs actions: write, beyond what
	// the merger App held until merge.checkReruns was turned on.
	RerunCheck(ctx context.Context, id int64) error
	Diff(ctx context.Context, base, head string) (Diff, error)
	Merge(ctx context.Context, nodeID, head string) error
	EnableAutoMerge(ctx context.Context, nodeID, expectedHeadSHA string) error
	DisableAutoMerge(ctx context.Context, nodeID string) error
	RevertPR(ctx context.Context, nodeID, title, body string) (Revert, error)
	// AddLabels and PullsWithLabel are the merge breaker's ledger on GitHub (v2 D1): the merger
	// labels what it merged and the reverts it opened, so the breaker's state survives a rebuild
	// that loses every Task, and only the merger's key writes it. It needs issues: write, GitHub's
	// permission for a pull request's labels.
	AddLabels(ctx context.Context, number int, labels ...string) error
	PullsWithLabel(ctx context.Context, label string) ([]LabelledPull, error)
	// WorkflowRuns, RunArtifact and FileAt are the dead-links probe's reads (merge gate MC5):
	// the detector's runs on main, their findings, and a finding's file there.
	WorkflowRuns(ctx context.Context, workflow string) ([]WorkflowRun, error)
	RunArtifact(ctx context.Context, runID int64, name, file string) ([]byte, error)
	FileAt(ctx context.Context, path, ref string) (string, error)
	// MergedSince, PullFiles and BlameAtParent are the monitor's follow-up reads: the pull
	// requests merged since a time, one pull request's patches, and who last changed a file's
	// lines before a merge. All three fail closed: an incomplete answer is an error.
	//
	// BlameAtParent blames at mergeSHA's first parent, which is the base only for a squash merge
	// (the factory's); a rebase merge's first parent is the previous rebased commit. The caller
	// blames at the follow-up's own merge commit, with FilePatch.BlamePath, so a file the original
	// pull request created is blamed where it exists. ErrNoBlame means nothing could be blamed.
	MergedSince(ctx context.Context, since time.Time) ([]MergedPull, error)
	PullFiles(ctx context.Context, number int) ([]FilePatch, error)
	BlameAtParent(ctx context.Context, mergeSHA, path string) ([]BlameRange, error)
}

var _ Merger = (*GitHub)(nil)
