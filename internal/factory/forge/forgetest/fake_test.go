// SPDX-License-Identifier: Apache-2.0

package forgetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Smana/agent-platform/internal/factory/forge"
)

// api is the method set the factory's consumers declare (narrate, intake, reconciler): the
// GitHub adapter and the fake must never drift apart.
type api interface {
	Labeled(ctx context.Context, label string) ([]forge.Item, error)
	LabelEvents(ctx context.Context, number int, label string) ([]forge.LabelEvent, error)
	Issue(ctx context.Context, number int) (forge.Issue, error)
	Comment(ctx context.Context, number int, body string) error
	RecentComments(ctx context.Context, number int) ([]forge.Comment, error)
	AddLabels(ctx context.Context, number int, labels ...string) error
	RemoveLabel(ctx context.Context, number int, label string) error
	PullRequestForBranch(ctx context.Context, branch string) (int, error)
	PullRequest(ctx context.Context, number int) (forge.PR, error)
}

var _ api = (*Fake)(nil)

func TestFakeRecordsWhatTheFactoryDid(t *testing.T) {
	ctx := t.Context()
	at := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	f := NewFake()
	f.Now = func() time.Time { return at }
	f.SetLabeled("factory/ready", forge.Item{Number: 7}, forge.Item{Number: 9, PullRequest: true})
	f.SetIssue(forge.Issue{Number: 7, Title: "Fix the link"})
	f.SetPR(forge.PR{Number: 12, HeadRef: "agent/3buqdlot"})
	f.SetBranch("agent/3buqdlot", 12)
	f.SetEvents(7, forge.LabelEvent{Actor: "Smana", Label: "factory/ready", At: at})
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
	if evs, err := f.LabelEvents(ctx, 7, "factory/ready"); len(evs) != 1 || !errors.Is(err, forge.ErrEventsTruncated) {
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
	if d, err := f.Diff(ctx, "base", "head"); err != nil || d.Base["docs/a.md"] != "old" || d.Head["docs/a.md"] != "new" ||
		len(d.Files) != 1 || d.Files[0] != (forge.FileStat{Path: "docs/a.md", Status: "modified", Additions: 1, Deletions: 1}) {
		t.Errorf("diff %+v %v", d, err)
	}
}

// The amendment (external review R02, ruling R52): a decision merges with expectedHeadOid, and
// GitHub checks the head at merge time. The fake must refuse a moved head the same way — and
// nothing may merge on it.
func TestFMergeRefusesAMovedHead(t *testing.T) {
	ctx := t.Context()
	f := NewFake()
	f.SetPR(forge.PR{Number: 12, NodeID: "PR_12", State: "OPEN", HeadSHA: "abc"})
	if err := f.Merge(ctx, "PR_12", "def"); !errors.Is(err, forge.ErrHeadMoved) {
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

// A created issue is numbered above every seeded one, and creates and closes can be made to fail.
func TestFakeIssuesNeverCollideAndCanFail(t *testing.T) {
	f := NewFake()
	f.SetIssue(forge.Issue{Number: 501, Title: "seeded"})
	iss, err := f.CreateIssue(t.Context(), "t", "b", nil)
	if err != nil || iss.Number <= 501 {
		t.Fatalf("%+v %v", iss, err)
	}
	boom := errors.New("boom")
	f.FailCreates(boom)
	f.FailCloses(boom)
	if _, err := f.CreateIssue(t.Context(), "t", "b", nil); !errors.Is(err, boom) {
		t.Errorf("create: %v", err)
	}
	if err := f.CloseIssue(t.Context(), iss.Number); !errors.Is(err, boom) {
		t.Errorf("close: %v", err)
	}
}
