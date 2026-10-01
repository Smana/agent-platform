// SPDX-License-Identifier: Apache-2.0

// Package forge is the factory's view of GitHub, through the factory's own App (C6): one
// repository per instance (OD-6). GitHub is the adapter, Fake the double for the packages that
// consume it; each consumer declares the interface it needs (AGENTS.md), so this package exports
// none. Logins are REST-shaped: a bot is always "name[bot]", from REST or from GraphQL.
package forge

import (
	"errors"
	"strings"
	"time"
)

// ErrEventsTruncated is LabelEvents reaching its page cap: the newest events are unread.
var ErrEventsTruncated = errors.New("forge: label events past the page cap are unread")

// Item is an open issue or pull request carrying a label.
type Item struct {
	Number      int
	PullRequest bool
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

// PR is one GraphQL snapshot of a pull request. Checks and statuses are a separate query in
// phase 7: reading them needs checks and statuses read, which only the merger App holds (R16).
type PR struct {
	Number         int
	NodeID         string
	URL            string
	Title          string
	State          string // OPEN | CLOSED | MERGED
	Author         string
	HeadRef        string
	HeadSHA        string
	HeadMessage    string
	MergedBy       string
	MergeCommitSHA string
	AutoMerge      bool
	Labels         []string
	Reviews        []Review
	Comments       []Comment
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
