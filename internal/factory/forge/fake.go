// SPDX-License-Identifier: Apache-2.0

package forge

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

// Fake is an in-memory forge for the tests of the packages that consume one. It has GitHub's
// method set; Now stamps its comments (time.Now when nil).
type Fake struct {
	Now func() time.Time

	mu           sync.Mutex
	labeled      map[string][]Item
	events       map[int][]LabelEvent
	issues       map[int]Issue
	prs          map[int]PR
	branches     map[string]int
	comments     map[int][]Comment
	added        map[int][]string
	removed      map[int][]string
	cut          map[int]bool
	closed       map[int]bool
	agentPulls   []AgentPull
	checks       map[int]Checks
	commitChecks map[string][]Check
	open         []PRSummary
	armed        []string
	disarmed     []string
	reverts      []string
	calls        []string
	nextID       int64
}

// NewFake returns an empty Fake.
func NewFake() *Fake {
	return &Fake{labeled: map[string][]Item{}, events: map[int][]LabelEvent{}, issues: map[int]Issue{},
		prs: map[int]PR{}, branches: map[string]int{}, comments: map[int][]Comment{}, added: map[int][]string{},
		removed: map[int][]string{}, cut: map[int]bool{}, closed: map[int]bool{},
		checks: map[int]Checks{}, commitChecks: map[string][]Check{}}
}

// SetLabeled sets the items Labeled returns for label.
func (f *Fake) SetLabeled(label string, items ...Item) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.labeled[label] = items
}

// SetEvents sets issue n's label events.
func (f *Fake) SetEvents(n int, evs ...LabelEvent) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events[n] = evs
}

// SetIssue stores an issue under its number.
func (f *Fake) SetIssue(i Issue) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issues[i.Number] = i
}

// SetPR stores a pull request under its number.
func (f *Fake) SetPR(p PR) {
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
func (f *Fake) SetAgentPulls(ps ...AgentPull) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.agentPulls = slices.Clone(ps)
}

// AgentPulls implements the forge's AgentPulls.
func (f *Fake) AgentPulls(context.Context) ([]AgentPull, error) {
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
func (f *Fake) Labeled(_ context.Context, label string) ([]Item, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.labeled[label]), nil
}

// LabelEvents implements the forge's LabelEvents.
func (f *Fake) LabelEvents(_ context.Context, n int, label string) ([]LabelEvent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []LabelEvent
	for _, e := range f.events[n] {
		if e.Label == label {
			out = append(out, e)
		}
	}
	if f.cut[n] {
		return out, ErrEventsTruncated
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
func (f *Fake) Issue(_ context.Context, n int) (Issue, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i, ok := f.issues[n]
	if !ok {
		return Issue{}, fmt.Errorf("issue %d not found", n)
	}
	return i, nil
}

// Comment implements the forge's Comment, as the factory App.
func (f *Fake) Comment(_ context.Context, n int, body string) error {
	now := time.Now
	if f.Now != nil {
		now = f.Now
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.comments[n] = append(f.comments[n], Comment{ID: f.nextID, Author: "ogenki-agent-factory[bot]", Body: body, At: now()})
	return nil
}

// RecentComments implements the forge's RecentComments.
func (f *Fake) RecentComments(_ context.Context, n int) ([]Comment, error) {
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
	f.labeled[label] = slices.DeleteFunc(f.labeled[label], func(i Item) bool { return i.Number == n })
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
func (f *Fake) SetComments(n int, cs ...Comment) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.comments[n] = slices.Clone(cs)
}

// PullRequest implements the forge's PullRequest.
func (f *Fake) PullRequest(_ context.Context, n int) (PR, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p, ok := f.prs[n]
	if !ok {
		return PR{}, fmt.Errorf("pull request %d not found", n)
	}
	return p, nil
}

// SetChecks sets pull request n's head-commit rollup, as PullRequestChecks returns it.
func (f *Fake) SetChecks(n int, c Checks) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.checks[n] = c
}

// SetCommitChecks sets the check runs sha carries, as CommitChecks returns them.
func (f *Fake) SetCommitChecks(sha string, cs ...Check) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commitChecks[sha] = cs
}

// SetOpenPRs sets the open pull requests OpenPullRequests returns.
func (f *Fake) SetOpenPRs(ps ...PRSummary) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.open = ps
}

// Armed are the node ids EnableAutoMerge was called with, in order.
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

// PullRequestChecks implements the Merger's PullRequestChecks.
func (f *Fake) PullRequestChecks(_ context.Context, n int) (Checks, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.checks[n], nil
}

// CommitChecks implements the Merger's CommitChecks.
func (f *Fake) CommitChecks(_ context.Context, sha string) ([]Check, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.commitChecks[sha]), nil
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
func (f *Fake) RevertPR(_ context.Context, nodeID, title, _ string) (Revert, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reverts = append(f.reverts, nodeID+" "+title)
	return Revert{Number: 900 + len(f.reverts), URL: "https://github.com/Smana/cloud-native-ref/pull/901", NodeID: "PR_revert"}, nil
}

// OpenPullRequests implements the forge's OpenPullRequests.
func (f *Fake) OpenPullRequests(context.Context) ([]PRSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.open), nil
}
