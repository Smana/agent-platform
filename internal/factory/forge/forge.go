// SPDX-License-Identifier: Apache-2.0

// Package forge is the factory's view of GitHub, through the factory's own App (C6): one
// repository per instance (OD-6). GitHub is the adapter, Fake the double for the packages that
// consume it; each consumer declares the interface it needs (AGENTS.md) — Merger is the one
// exception the owner ruled (2026-09-27; R16), because its key must reach only the factory, and a
// seam defined here keeps it out of every consumer's hands. Logins are REST-shaped: a bot is
// always "name[bot]", from REST or from GraphQL.
package forge

import (
	"context"
	"errors"
	"strings"
	"time"
)

// ErrEventsTruncated is LabelEvents reaching its page cap: the newest events are unread.
var ErrEventsTruncated = errors.New("forge: label events past the page cap are unread")

// ErrHeadMoved and ErrNotMergeable are GitHub's refusals of a merge at merge time (ruling R52):
// expectedHeadOid is no longer the head — a push landed after the decision (HTTP 409) — or the
// pull request cannot be merged as it stands: protections, an approval it lacks (HTTP 405).
// Both are decisions the factory can make, so they are typed; anything else is a plain error.
var (
	ErrHeadMoved    = errors.New("forge: the pull request's head moved since the decision")
	ErrNotMergeable = errors.New("forge: GitHub does not allow the merge as it stands")
)

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
	Number       int
	URL          string
	Title        string
	Body         string
	State        string // OPEN | CLOSED
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
	State       string // OPEN | CLOSED | MERGED
	Author      string
	HeadRef     string
	HeadSHA     string
	HeadMessage string
	// The head commit's committer date: set by whoever committed, so a claim like Trailer's.
	HeadCommittedAt time.Time
	MergedBy        string
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
// It is a claim, never authentication (review B1). The harness's commit-msg hook runs
// `git interpret-trailers --if-exists doNothing`, and git matches keys case-insensitively, so an
// agent that writes its own Agent-Run (or agent-run) keeps the real one out; an agent with a shell
// can skip the hook altogether. Tasks 7.2/7.3 must fail closed on an absent, foreign, duplicated
// or case-variant Agent-Run, and a matching one proves only that the commit claims to be the run's.
func (p PR) Trailer(key string) string {
	msg := strings.TrimRight(strings.ReplaceAll(p.HeadMessage, "\r\n", "\n"), " \t\n")
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

// Check is one CI check run on a commit: SUCCESS, FAILURE or PENDING.
type Check struct {
	Name  string
	State string // SUCCESS | FAILURE | PENDING
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

// Revert is the pull request RevertPR opened.
type Revert struct {
	Number int
	URL    string
	NodeID string
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
	Files(ctx context.Context, base, head string) (baseFiles, headFiles map[string]string, err error)
	Merge(ctx context.Context, nodeID, head string) error
	EnableAutoMerge(ctx context.Context, nodeID, expectedHeadSHA string) error
	DisableAutoMerge(ctx context.Context, nodeID string) error
	RevertPR(ctx context.Context, nodeID, title, body string) (Revert, error)
}

var (
	_ Merger = (*GitHub)(nil)
	_ Merger = (*Fake)(nil)
)
