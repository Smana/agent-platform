// SPDX-License-Identifier: Apache-2.0

// Package forgetest holds the in-memory forge fake for the tests of the packages that consume
// one. Only tests import it: the factory binary never links it.
package forgetest

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/Smana/agent-platform/internal/factory/forge"
)

// The fake has the Merger's method set too.
var _ forge.Merger = (*Fake)(nil)

// Fake is an in-memory forge for the tests of the packages that consume one. It has GitHub's
// method set; Now stamps its comments (time.Now when nil).
type Fake struct {
	Now func() time.Time

	mu           sync.Mutex
	labeled      map[string][]forge.Item
	events       map[int][]forge.LabelEvent
	issues       map[int]forge.Issue
	prs          map[int]forge.PR
	branches     map[string]int
	comments     map[int][]forge.Comment
	added        map[int][]string
	removed      map[int][]string
	cut          map[int]bool
	closed       map[int]bool
	agentPulls   []forge.AgentPull
	checks       map[int]forge.Checks
	commitChecks map[string][]forge.Check
	commits      map[int][]forge.Commit
	commitsCut   map[int]bool
	activity     map[string][]forge.Push
	activityErr  map[string]error
	filesBase    map[string]string
	filesHead    map[string]string
	fileStats    []forge.FileStat
	runs         []forge.WorkflowRun
	artifacts    map[int64][]byte
	mainFiles    map[string]string
	diffErr      error
	open         []forge.PRSummary
	created      []string
	armed        []string
	reruns       []int64
	rerunFail    map[int64]error
	disarmed     []string
	reverts      []string
	refused      map[string]error
	labelErr     error
	labelErrFor  map[string]error
	files        map[string]string // default-branch files by path; "!" fails the read
	calls        []string
	merged       []forge.MergedPull
	pullFiles    map[int][]forge.FilePatch
	blame        map[string][]forge.BlameRange
	blameCalls   int
	mergedErr    error
	filesErr     map[int]error
	blameErr     map[string]error
	subErr       error
	createErr    error
	closeErr     error
	nextID       int64
}

// NewFake returns an empty Fake.
func NewFake() *Fake {
	return &Fake{labeled: map[string][]forge.Item{}, events: map[int][]forge.LabelEvent{}, issues: map[int]forge.Issue{},
		prs: map[int]forge.PR{}, branches: map[string]int{}, comments: map[int][]forge.Comment{}, added: map[int][]string{},
		removed: map[int][]string{}, cut: map[int]bool{}, closed: map[int]bool{},
		checks: map[int]forge.Checks{}, commitChecks: map[string][]forge.Check{}, commits: map[int][]forge.Commit{}, commitsCut: map[int]bool{},
		activity: map[string][]forge.Push{}, activityErr: map[string]error{}}
}

// SetLabeled sets the items Labeled returns for label.
func (f *Fake) SetLabeled(label string, items ...forge.Item) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.labeled[label] = items
}

// SetEvents sets issue n's label events.
func (f *Fake) SetEvents(n int, evs ...forge.LabelEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events[n] = evs
}

// SetIssue stores an issue under its number.
func (f *Fake) SetIssue(i forge.Issue) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issues[i.Number] = i
}

// SetPR stores a pull request under its number.
func (f *Fake) SetPR(p forge.PR) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prs[p.Number] = p
}

// SetBranch makes n the pull request of branch.
func (f *Fake) SetBranch(branch string, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.branches[branch] = n
}

// SetAgentPulls sets the open agent-branch pull requests AgentPulls returns (R51's orphan scan).
func (f *Fake) SetAgentPulls(ps ...forge.AgentPull) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.agentPulls = slices.Clone(ps)
}

// AgentPulls implements the forge's AgentPulls.
func (f *Fake) AgentPulls(context.Context) ([]forge.AgentPull, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.agentPulls), nil
}

// Comments are the bodies posted on n, oldest first.
func (f *Fake) Comments(n int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, c := range f.comments[n] {
		out = append(out, c.Body)
	}
	return out
}

// Added are the labels added to n.
func (f *Fake) Added(n int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.added[n])
}

// Removed are the labels removed from n.
func (f *Fake) Removed(n int) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.removed[n])
}

// Labeled implements the forge's Labeled.
func (f *Fake) Labeled(_ context.Context, label string) ([]forge.Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.labeled[label]), nil
}

// LabelEvents implements the forge's LabelEvents.
func (f *Fake) LabelEvents(_ context.Context, n int, label string) ([]forge.LabelEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []forge.LabelEvent
	for _, e := range f.events[n] {
		if e.Label == label {
			out = append(out, e)
		}
	}
	if f.cut[n] {
		return out, forge.ErrEventsTruncated
	}
	return out, nil
}

// SetEventsTruncated makes issue n's LabelEvents report the page cap, as GitHub does past it.
func (f *Fake) SetEventsTruncated(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cut[n] = true
}

// Issue implements the forge's Issue.
func (f *Fake) Issue(_ context.Context, n int) (forge.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i, ok := f.issues[n]
	if !ok {
		return forge.Issue{}, fmt.Errorf("issue %d not found", n)
	}
	return i, nil
}

// now is Now's reading, time.Now's when Now is nil.
func (f *Fake) now() time.Time {
	if f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

// Comment implements the forge's Comment, as the factory App.
func (f *Fake) Comment(_ context.Context, n int, body string) error {
	now := f.now()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.comments[n] = append(f.comments[n], forge.Comment{ID: f.nextID, Author: "ogenki-agent-factory[bot]", Body: body, At: now})
	return nil
}

// RecentComments implements the forge's RecentComments.
func (f *Fake) RecentComments(_ context.Context, n int) ([]forge.Comment, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.comments[n]), nil
}

// AddLabels implements the forge's AddLabels, and the labels show on the stored agent pulls, as
// they do on GitHub — a scan that re-reads what an earlier one labelled.
func (f *Fake) AddLabels(_ context.Context, n int, labels ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.added[n] = append(f.added[n], labels...)
	f.calls = append(f.calls, fmt.Sprintf("add-labels %d %s", n, strings.Join(labels, ",")))
	if p, ok := f.prs[n]; ok {
		p.Labels = append(slices.Clone(p.Labels), labels...)
		f.prs[n] = p
	}
	for i := range f.agentPulls {
		if f.agentPulls[i].Number == n {
			f.agentPulls[i].Labels = append(f.agentPulls[i].Labels, labels...)
		}
	}
	return nil
}

// RemoveLabel implements the forge's RemoveLabel, and takes n off that label's listing.
func (f *Fake) RemoveLabel(_ context.Context, n int, label string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removed[n] = append(f.removed[n], label)
	f.calls = append(f.calls, fmt.Sprintf("remove-label %d %s", n, label))
	f.labeled[label] = slices.DeleteFunc(f.labeled[label], func(i forge.Item) bool { return i.Number == n })
	return nil
}

// PullRequestForBranch implements the forge's PullRequestForBranch.
func (f *Fake) PullRequestForBranch(_ context.Context, branch string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.branches[branch], nil
}

// ClosePR implements the forge's ClosePR. The stored pull request, if any, reads CLOSED after it.
func (f *Fake) ClosePR(_ context.Context, n int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed[n] = true
	f.calls = append(f.calls, fmt.Sprintf("close %d", n))
	if p, ok := f.prs[n]; ok {
		p.State = "CLOSED"
		f.prs[n] = p
	}
	return nil
}

// CreateIssue implements the forge's CreateIssue: it records "title\nbody\nlabels: a,b", stores the
// issue as the factory App's, open, numbered from 501 with an id 9000 more, and returns it.
func (f *Fake) CreateIssue(_ context.Context, title, body string, labels []string) (forge.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createErr != nil {
		return forge.Issue{}, f.createErr
	}
	f.created = append(f.created, title+"\n"+body+"\nlabels: "+strings.Join(labels, ","))
	n := 500 + len(f.created)
	for k := range f.issues { // above every seeded issue: a number is never reused
		n = max(n, k+1)
	}
	i := forge.Issue{Number: n, ID: int64(9000 + n), Title: title, Body: body, State: "OPEN",
		Author: "ogenki-agent-factory[bot]", Labels: slices.Clone(labels)}
	f.issues[n] = i
	return i, nil
}

// IssuesLabelled implements the forge's IssuesLabelled: every stored issue carrying label, by number.
func (f *Fake) IssuesLabelled(_ context.Context, label string) ([]forge.Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []forge.Issue
	for _, n := range slices.Sorted(maps.Keys(f.issues)) {
		if slices.Contains(f.issues[n].Labels, label) {
			out = append(out, f.issues[n])
		}
	}
	return out, nil
}

// AddSubIssue implements the forge's AddSubIssue: "sub-issue <parent> <id>" in Calls, or FailSubIssues' error.
func (f *Fake) AddSubIssue(_ context.Context, parent int, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("sub-issue %d %d", parent, id))
	return f.subErr
}

// FailSubIssues makes every AddSubIssue fail with err.
func (f *Fake) FailSubIssues(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subErr = err
}

// FailCreates makes every CreateIssue fail with err.
func (f *Fake) FailCreates(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createErr = err
}

// FailCloses makes every CloseIssue fail with err.
func (f *Fake) FailCloses(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closeErr = err
}

// CloseIssue implements the forge's CloseIssue: the stored issue reads CLOSED, "close-issue n" in Calls.
func (f *Fake) CloseIssue(_ context.Context, n int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closeErr != nil {
		return f.closeErr
	}
	f.calls = append(f.calls, fmt.Sprintf("close-issue %d", n))
	if i, ok := f.issues[n]; ok {
		i.State = "CLOSED"
		f.issues[n] = i
	}
	return nil
}

// Created are the issues CreateIssue recorded, in order, for tests of what the factory proposed.
func (f *Fake) Created() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.created)
}

// Calls are the label and close calls made, in order ("add-labels 12 a,b", "remove-label 12 a",
// "close 12"), for tests of what must happen before what.
func (f *Fake) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// Closed is whether ClosePR closed n.
func (f *Fake) Closed(n int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed[n]
}

// SetComments replaces a thread's comments, for commands written by other users.
func (f *Fake) SetComments(n int, cs ...forge.Comment) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.comments[n] = slices.Clone(cs)
}

// PullRequest implements the forge's PullRequest.
func (f *Fake) PullRequest(_ context.Context, n int) (forge.PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.prs[n]
	if !ok {
		return forge.PR{}, fmt.Errorf("pull request %d not found", n)
	}
	return p, nil
}

// SetCommits sets pull request n's commits, oldest first, as PullRequestCommits returns them.
func (f *Fake) SetCommits(n int, cs ...forge.Commit) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commits[n] = slices.Clone(cs)
}

// SetCommitsTruncated makes PullRequestCommits on n report GitHub's 250-commit cap.
func (f *Fake) SetCommitsTruncated(n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commitsCut[n] = true
}

// PullRequestCommits implements the forge's PullRequestCommits. Unset, a pull request's commits
// are its head alone, so a test that never pushes more reads what GitHub would.
func (f *Fake) PullRequestCommits(_ context.Context, n int) ([]forge.Commit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	cs, ok := f.commits[n]
	if !ok {
		p, found := f.prs[n]
		if !found {
			return nil, fmt.Errorf("pull request %d not found", n)
		}
		cs = []forge.Commit{{SHA: p.HeadSHA, Author: p.Author, Message: p.HeadMessage}}
	}
	if f.commitsCut[n] {
		return slices.Clone(cs), forge.ErrCommitsTruncated
	}
	return slices.Clone(cs), nil
}

// SetActivity sets branch's activity, newest first, as BranchActivity returns it.
func (f *Fake) SetActivity(branch string, ps ...forge.Push) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.activity[branch] = slices.Clone(ps)
}

// SetActivityErr makes BranchActivity on branch fail with err (ErrActivityTruncated: past the cap).
func (f *Fake) SetActivityErr(branch string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.activityErr[branch] = err
}

// BranchActivity implements the forge's BranchActivity. Unset, a branch is its pull request's
// head, created by the pull request's author — the agents App in the reconciler's rigs — so the
// push check passes: a test that never calls SetActivity does not exercise it (#76's tests do).
func (f *Fake) BranchActivity(_ context.Context, branch string) ([]forge.Push, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.activityErr[branch]; err != nil {
		return slices.Clone(f.activity[branch]), err
	}
	if ps, ok := f.activity[branch]; ok {
		return slices.Clone(ps), nil
	}
	for _, p := range f.prs {
		if p.HeadRef == branch {
			return []forge.Push{{Type: "branch_creation", Actor: p.Author, Before: strings.Repeat("0", 40), After: p.HeadSHA}}, nil
		}
	}
	return nil, nil
}

// SetChecks sets pull request n's head-commit rollup, as PullRequestChecks returns it.
func (f *Fake) SetChecks(n int, c forge.Checks) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks[n] = c
}

// SetCommitChecks sets the check runs sha carries, as CommitChecks returns them.
func (f *Fake) SetCommitChecks(sha string, cs ...forge.Check) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commitChecks[sha] = cs
}

// SetFiles sets what Files returns: only the files a compare says changed, the base side as of
// the merge base, each side "" when the file is absent there, as an added or deleted file reads
// on GitHub.
func (f *Fake) SetFiles(base, head map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.filesBase, f.filesHead = maps.Clone(base), maps.Clone(head)
}

// SetDiffError makes Diff fail with err, such as forge.ErrDiffTooLarge.
func (f *Fake) SetDiffError(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.diffErr = err
}

// SetRuns sets the workflow runs WorkflowRuns returns, and each run's findings artifact.
func (f *Fake) SetRuns(runs []forge.WorkflowRun, artifacts map[int64][]byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs, f.artifacts = slices.Clone(runs), maps.Clone(artifacts)
}

// SetMainFiles sets the files FileAt reads, at any ref.
func (f *Fake) SetMainFiles(files map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mainFiles = maps.Clone(files)
}

// WorkflowRuns implements the Merger's WorkflowRuns.
func (f *Fake) WorkflowRuns(context.Context, string) ([]forge.WorkflowRun, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.runs), nil
}

// RunArtifact implements the Merger's RunArtifact: the run's artifact as SetRuns gave it.
func (f *Fake) RunArtifact(_ context.Context, id int64, name, _ string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.artifacts[id]
	if !ok {
		return nil, fmt.Errorf("forgetest: run %d has no artifact %q", id, name)
	}
	return b, nil
}

// FileAt implements the Merger's FileAt.
func (f *Fake) FileAt(_ context.Context, path, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.mainFiles[path], nil
}

// SetFileStats sets the changed-file list Diff returns in place of the one SetFiles implies.
func (f *Fake) SetFileStats(fs ...forge.FileStat) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fileStats = slices.Clone(fs)
}

// Diff implements the Merger's Diff: SetFiles' two sides, and SetFileStats' list, or else the
// one they imply: a side "" is an added or removed file, and each line that differs at its
// position counts as one addition and one deletion. These stats are approximate: tests that
// depend on exact counts set them with SetFileStats.
func (f *Fake) Diff(context.Context, string, string) (forge.Diff, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.diffErr != nil {
		return forge.Diff{}, f.diffErr
	}
	d := forge.Diff{Base: maps.Clone(f.filesBase), Head: maps.Clone(f.filesHead), Files: slices.Clone(f.fileStats)}
	if d.Files != nil {
		return d, nil
	}
	paths := slices.Sorted(maps.Keys(d.Base))
	for p := range d.Head {
		if _, ok := d.Base[p]; !ok {
			paths = append(paths, p)
		}
	}
	slices.Sort(paths)
	for _, p := range paths {
		b, h := d.Base[p], d.Head[p]
		st := forge.FileStat{Path: p, Status: "modified"}
		switch {
		case b == "":
			st.Status = "added"
		case h == "":
			st.Status = "removed"
		}
		bl, hl := strings.Split(b, "\n"), strings.Split(h, "\n")
		for i := range max(len(bl), len(hl)) {
			if i < len(bl) && i < len(hl) && bl[i] == hl[i] {
				continue
			}
			if i < len(hl) && h != "" {
				st.Additions++
			}
			if i < len(bl) && b != "" {
				st.Deletions++
			}
		}
		d.Files = append(d.Files, st)
	}
	return d, nil
}

// SetOpenPRs sets the open pull requests OpenPullRequests returns.
func (f *Fake) SetOpenPRs(ps ...forge.PRSummary) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.open = ps
}

// Armed are the mergings and arming recorded, in order: Merge appends "nodeID sha" — the head
// its merge was pinned to — and EnableAutoMerge the bare nodeID it armed (a revert, §6.4).
func (f *Fake) Armed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.armed)
}

// Disarmed are the node ids DisableAutoMerge was called with, in order.
func (f *Fake) Disarmed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.disarmed)
}

// Reverts are the "<nodeID> <title>" pairs RevertPR was called with, in order.
func (f *Fake) Reverts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.reverts)
}

// RefuseMerge makes the next Merge of nodeID fail with err — the race GitHub's merge-time check
// catches between reading the pull request and merging it (R52). One-shot: the merge after it
// runs the fake's own head check.
func (f *Fake) RefuseMerge(nodeID string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.refused == nil {
		f.refused = map[string]error{}
	}
	f.refused[nodeID] = err
}

// Merge implements the Merger's Merge. Like GitHub checking the head at merge time (R52), a
// stored pull request whose head is no longer head is refused and stays open: nothing merges
// on a moved head.
func (f *Fake) Merge(_ context.Context, nodeID, head string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.refused[nodeID]; ok {
		delete(f.refused, nodeID)
		return err
	}
	for n, p := range f.prs {
		if p.NodeID != nodeID {
			continue
		}
		if p.HeadSHA != head {
			return fmt.Errorf("forge: fake: %w: %s is at %s, not %s", forge.ErrHeadMoved, nodeID, p.HeadSHA, head)
		}
		p.State, p.MergeCommitSHA, p.MergedBy, p.MergedAt = "MERGED", head, "ogenki-agent-merger[bot]", f.now()
		f.prs[n] = p
		break
	}
	f.armed = append(f.armed, nodeID+" "+head)
	return nil
}

// PullRequestChecks implements the Merger's PullRequestChecks.
func (f *Fake) PullRequestChecks(_ context.Context, n int) (forge.Checks, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.checks[n], nil
}

// CommitChecks implements the Merger's CommitChecks.
func (f *Fake) CommitChecks(_ context.Context, sha string) ([]forge.Check, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.commitChecks[sha]), nil
}

// RerunCheck implements the Merger's RerunCheck, recording the check run id.
func (f *Fake) RerunCheck(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reruns = append(f.reruns, id)
	return f.rerunFail[id]
}

// FailReruns makes RerunCheck answer err for these check run ids; with none it clears them.
func (f *Fake) FailReruns(err error, ids ...int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rerunFail = map[int64]error{}
	for _, id := range ids {
		f.rerunFail[id] = err
	}
}

// Reruns are the check run ids RerunCheck was called with, in order.
func (f *Fake) Reruns() []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.reruns)
}

// EnableAutoMerge implements the Merger's EnableAutoMerge, recording the arming. The expected
// head is the caller's decision, not state the fake keeps.
func (f *Fake) EnableAutoMerge(_ context.Context, nodeID, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.armed = append(f.armed, nodeID)
	return nil
}

// DisableAutoMerge implements the Merger's DisableAutoMerge, recording the disarming.
func (f *Fake) DisableAutoMerge(_ context.Context, nodeID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disarmed = append(f.disarmed, nodeID)
	return nil
}

// RevertPR implements the Merger's RevertPR: every call opens the next numbered revert.
func (f *Fake) RevertPR(_ context.Context, nodeID, title, _ string) (forge.Revert, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reverts = append(f.reverts, nodeID+" "+title)
	return forge.Revert{Number: 900 + len(f.reverts), URL: "https://github.com/Smana/cloud-native-ref/pull/901", NodeID: "PR_revert"}, nil
}

// FailLabelReads makes PullsWithLabel answer err; nil clears it.
func (f *Fake) FailLabelReads(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.labelErr = err
}

// FailLabelReadsFor makes PullsWithLabel answer err for label alone; nil clears it.
func (f *Fake) FailLabelReadsFor(label string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.labelErrFor == nil {
		f.labelErrFor = map[string]error{}
	}
	f.labelErrFor[label] = err
}

// PullsWithLabel implements the Merger's PullsWithLabel over the stored pull requests, newest
// number first.
func (f *Fake) PullsWithLabel(_ context.Context, label string) ([]forge.LabelledPull, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.labelErr != nil {
		return nil, f.labelErr
	}
	if err := f.labelErrFor[label]; err != nil {
		return nil, err
	}
	var out []forge.LabelledPull
	for _, n := range slices.Sorted(maps.Keys(f.prs)) {
		if p := f.prs[n]; slices.Contains(p.Labels, label) {
			out = append(out, forge.LabelledPull{Number: n, State: p.State, MergedAt: p.MergedAt})
		}
	}
	slices.Reverse(out)
	return out, nil
}

// OpenPullRequests implements the forge's OpenPullRequests.
func (f *Fake) OpenPullRequests(context.Context) ([]forge.PRSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.open), nil
}

// NotAFile as a SetFile text makes the read fail with forge.ErrNotAFile (a directory, a symlink).
const NotAFile = "<not-a-file>"

// SetFile sets path's text on the default branch; "!" makes its read fail, NotAFile makes it unreadable for good.
func (f *Fake) SetFile(path, text string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.files == nil {
		f.files = map[string]string{}
	}
	f.files[path] = text
}

// DefaultBranchFile is path's text on the default branch, with a sha derived from it.
func (f *Fake) DefaultBranchFile(_ context.Context, path string) (string, string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	text, ok := f.files[path]
	switch {
	case !ok:
		return "", "", false, nil
	case text == "!":
		return "", "", false, errors.New("forgetest: read failed")
	case text == NotAFile:
		return "", "", false, forge.ErrNotAFile
	}
	return text, fmt.Sprintf("%x", sha256.Sum256([]byte(text)))[:40], true, nil
}

// SetMerged sets the pull requests MergedSince filters by time.
func (f *Fake) SetMerged(ps ...forge.MergedPull) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.merged = ps
}

// FailMergedSince makes MergedSince answer err (forge.ErrMergedTruncated, say); nil clears it.
func (f *Fake) FailMergedSince(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mergedErr = err
}

// MergedSince implements the Merger's MergedSince: the set pull requests merged at or after since,
// which the search takes to the second.
func (f *Fake) MergedSince(_ context.Context, since time.Time) ([]forge.MergedPull, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.mergedErr != nil {
		return nil, f.mergedErr
	}
	since = since.Truncate(time.Second)
	var out []forge.MergedPull
	for _, p := range f.merged {
		if !p.MergedAt.Before(since) {
			out = append(out, p)
		}
	}
	return out, nil
}

// SetPullFiles sets the files of pull request n.
func (f *Fake) SetPullFiles(n int, fs ...forge.FilePatch) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pullFiles == nil {
		f.pullFiles = map[int][]forge.FilePatch{}
	}
	f.pullFiles[n] = fs
}

// FailPullFiles makes PullFiles of pull request n answer err; nil clears it.
func (f *Fake) FailPullFiles(n int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.filesErr == nil {
		f.filesErr = map[int]error{}
	}
	f.filesErr[n] = err
}

// PullFiles implements the Merger's PullFiles; a pull request never set is an error, as
// PullRequest's is.
func (f *Fake) PullFiles(_ context.Context, n int) ([]forge.FilePatch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.filesErr[n]; err != nil {
		return nil, err
	}
	fs, ok := f.pullFiles[n]
	if !ok {
		return nil, fmt.Errorf("pull request %d files not set", n)
	}
	return slices.Clone(fs), nil
}

// SetBlame sets the blame ranges of path at the parent of mergeSHA.
func (f *Fake) SetBlame(mergeSHA, path string, rs ...forge.BlameRange) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.blame == nil {
		f.blame = map[string][]forge.BlameRange{}
	}
	f.blame[mergeSHA+"\x00"+path] = rs
}

// FailBlame makes BlameAtParent of path at mergeSHA answer err (forge.ErrNoBlame, say); nil clears it.
func (f *Fake) FailBlame(mergeSHA, path string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.blameErr == nil {
		f.blameErr = map[string]error{}
	}
	f.blameErr[mergeSHA+"\x00"+path] = err
}

// BlameAtParent implements the Merger's BlameAtParent and counts the call; a file never set has
// nothing to blame: forge.ErrNoBlame.
func (f *Fake) BlameAtParent(_ context.Context, mergeSHA, path string) ([]forge.BlameRange, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.blameCalls++
	k := mergeSHA + "\x00" + path
	if err := f.blameErr[k]; err != nil {
		return nil, err
	}
	rs, ok := f.blame[k]
	if !ok {
		return nil, forge.ErrNoBlame
	}
	return slices.Clone(rs), nil
}

// BlameCalls is how many times BlameAtParent was called.
func (f *Fake) BlameCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.blameCalls
}
