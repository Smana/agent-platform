// SPDX-License-Identifier: Apache-2.0

// Package forgetest holds the in-memory forge fake for the tests of the packages that consume
// one. Only tests import it: the factory binary never links it.
package forgetest

import (
	"context"
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
	open         []forge.PRSummary
	created      []string
	armed        []string
	reruns       []int64
	rerunFail    map[int64]error
	disarmed     []string
	reverts      []string
	refused      map[string]error
	labelErr     error
	calls        []string
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

// CreateIssue implements the forge's CreateIssue, recording each issue as
// "title\nbody\nlabels: a,b" and returning the next number from 501.
func (f *Fake) CreateIssue(_ context.Context, title, body string, labels []string) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.created = append(f.created, title+"\n"+body+"\nlabels: "+strings.Join(labels, ","))
	return 500 + len(f.created), nil
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

// Files implements the Merger's Files.
func (f *Fake) Files(context.Context, string, string) (map[string]string, map[string]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return maps.Clone(f.filesBase), maps.Clone(f.filesHead), nil
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

// PullsWithLabel implements the Merger's PullsWithLabel over the stored pull requests, newest
// number first.
func (f *Fake) PullsWithLabel(_ context.Context, label string) ([]forge.LabelledPull, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.labelErr != nil {
		return nil, f.labelErr
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
