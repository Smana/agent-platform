// SPDX-License-Identifier: Apache-2.0

package forge

import (
	"context"
	"errors"
	"testing"
	"time"
)

// api is the method set the factory's consumers declare (narrate, intake, reconciler): the
// GitHub adapter and the fake must never drift apart.
type api interface {
	Labeled(ctx context.Context, label string) ([]Item, error)
	LabelEvents(ctx context.Context, number int, label string) ([]LabelEvent, error)
	Issue(ctx context.Context, number int) (Issue, error)
	Comment(ctx context.Context, number int, body string) error
	RecentComments(ctx context.Context, number int) ([]Comment, error)
	AddLabels(ctx context.Context, number int, labels ...string) error
	RemoveLabel(ctx context.Context, number int, label string) error
	PullRequestForBranch(ctx context.Context, branch string) (int, error)
	PullRequest(ctx context.Context, number int) (PR, error)
}

var (
	_ api = (*GitHub)(nil)
	_ api = (*Fake)(nil)
)

// Trailer reads only the message's last paragraph, where git interpret-trailers puts trailers, so
// a line in the body or the subject is never read as one. What it returns is a claim.
func TestTrailerReadsOnlyTheTrailerBlock(t *testing.T) {
	for name, c := range map[string]struct{ msg, want string }{
		"the trailer":             {"docs: fix a link\n\nAgent-Run: 7f3cq2xz", "7f3cq2xz"},
		"the last occurrence":     {"docs: fix\n\nAgent-Run: aaaaaaaa\nAgent-Run: 7f3cq2xz\n", "7f3cq2xz"},
		"CRLF":                    {"docs: fix\r\n\r\nAgent-Run: 7f3cq2xz\r\n", "7f3cq2xz"},
		"absent":                  {"docs: fix a link", ""},
		"a body line is not one":  {"docs: fix\n\nAgent-Run: spoofed1\n\nSigned-off-by: a <a@b>", ""},
		"the subject is not one":  {"Agent-Run: spoofed1", ""},
		"another key":             {"docs: fix\n\nAgent-Runs: aaaaaaaa", ""},
		"trailing blank lines":    {"docs: fix\n\nAgent-Run: 7f3cq2xz\n\n\n", "7f3cq2xz"},
		"a key is case-sensitive": {"docs: fix\n\nagent-run: 7f3cq2xz", ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := (PR{HeadMessage: c.msg}).Trailer("Agent-Run"); got != c.want {
				t.Errorf("Trailer = %q, want %q", got, c.want)
			}
		})
	}
}

// The review's B1: Trailer is what the head commit claims, never authentication. The harness
// hook runs `git interpret-trailers --if-exists doNothing`, so an agent that writes its own
// Agent-Run, in any case, keeps the hook from adding the real one; and an agent with a shell can
// skip the hook. Each case returns what the message says, and 7.2/7.3 must fail closed on it.
func TestTrailerIsAClaimNotAProof(t *testing.T) {
	for name, c := range map[string]struct{ msg, want string }{
		// The hook added nothing: the agent's own value is all there is.
		"a forged trailer": {"docs: fix\n\nAgent-Run: forged01", "forged01"},
		// Both lines are in the message; the last is returned and says nothing about the first.
		"a duplicated trailer": {"docs: fix\n\nAgent-Run: 7f3cq2xz\nAgent-Run: forged01", "forged01"},
		// git reads agent-run as Agent-Run, so the hook added nothing; the exact key is absent.
		"a case-variant trailer": {"docs: fix\n\nagent-run: forged01", ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := (PR{HeadMessage: c.msg}).Trailer("Agent-Run"); got != c.want {
				t.Errorf("Trailer = %q, want %q", got, c.want)
			}
		})
	}
}

func TestFakeRecordsWhatTheFactoryDid(t *testing.T) {
	ctx := t.Context()
	at := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	f := NewFake()
	f.Now = func() time.Time { return at }
	f.SetLabeled("factory/ready", Item{Number: 7}, Item{Number: 9, PullRequest: true})
	f.SetIssue(Issue{Number: 7, Title: "Fix the link"})
	f.SetPR(PR{Number: 12, HeadRef: "agent/3buqdlot"})
	f.SetBranch("agent/3buqdlot", 12)
	f.SetEvents(7, LabelEvent{Actor: "Smana", Label: "factory/ready", At: at})
	if err := f.Comment(ctx, 7, "hello"); err != nil {
		t.Fatal(err)
	}
	if err := f.AddLabels(ctx, 7, "factory/class:docs-links"); err != nil {
		t.Fatal(err)
	}
	if err := f.RemoveLabel(ctx, 7, "factory/ready"); err != nil {
		t.Fatal(err)
	}
	items, _ := f.Labeled(ctx, "factory/ready")
	if len(items) != 1 || items[0].Number != 9 {
		t.Errorf("a removed label leaves the listing: %v", items)
	}
	if got := f.Comments(7); len(got) != 1 || got[0] != "hello" {
		t.Errorf("comments %v", got)
	}
	cs, _ := f.RecentComments(ctx, 7)
	if len(cs) != 1 || cs[0].Author != "ogenki-agent-factory[bot]" || !cs[0].At.Equal(at) || cs[0].ID != 1 {
		t.Errorf("recent %+v", cs)
	}
	if a, r := f.Added(7), f.Removed(7); len(a) != 1 || len(r) != 1 {
		t.Errorf("added %v removed %v", a, r)
	}
	if n, _ := f.PullRequestForBranch(ctx, "agent/3buqdlot"); n != 12 {
		t.Errorf("branch → %d", n)
	}
	if evs, _ := f.LabelEvents(ctx, 7, "factory/ready"); len(evs) != 1 {
		t.Errorf("events %v", evs)
	}
	if evs, _ := f.LabelEvents(ctx, 7, "factory/stop"); len(evs) != 0 {
		t.Errorf("another label's events: %v", evs)
	}
	f.SetEventsTruncated(7)
	if evs, err := f.LabelEvents(ctx, 7, "factory/ready"); len(evs) != 1 || !errors.Is(err, ErrEventsTruncated) {
		t.Errorf("truncated: %v %v", evs, err)
	}
	if _, err := f.Issue(ctx, 8); err == nil {
		t.Error("an unknown issue is an error")
	}
	if _, err := f.PullRequest(ctx, 13); err == nil {
		t.Error("an unknown PR is an error")
	}
	if p, err := f.PullRequest(ctx, 12); err != nil || p.HeadRef != "agent/3buqdlot" {
		t.Errorf("%+v %v", p, err)
	}
	f.SetFiles(map[string]string{"docs/a.md": "old"}, map[string]string{"docs/a.md": "new"})
	if b, h, err := f.Files(ctx, "base", "head"); err != nil || b["docs/a.md"] != "old" || h["docs/a.md"] != "new" {
		t.Errorf("files %v %v %v", b, h, err)
	}
}

// The amendment (external review R02, ruling R52): a decision merges with expectedHeadOid, and
// GitHub checks the head at merge time. The fake must refuse a moved head the same way — and
// nothing may merge on it.
func TestFMergeRefusesAMovedHead(t *testing.T) {
	ctx := t.Context()
	f := NewFake()
	f.SetPR(PR{Number: 12, NodeID: "PR_12", State: "OPEN", HeadSHA: "abc"})
	if err := f.Merge(ctx, "PR_12", "def"); !errors.Is(err, ErrHeadMoved) {
		t.Fatalf("a moved head: %v", err)
	}
	if a := f.Armed(); len(a) != 0 {
		t.Fatalf("a refused merge is not a merge: %v", a)
	}
	if p, err := f.PullRequest(ctx, 12); err != nil || p.State != "OPEN" {
		t.Fatalf("nothing merged: %+v %v", p, err)
	}
	if err := f.Merge(ctx, "PR_12", "abc"); err != nil {
		t.Fatal(err)
	}
	if a := f.Armed(); len(a) != 1 || a[0] != "PR_12 abc" {
		t.Fatalf("the merge records the decided head: %v", a)
	}
}
