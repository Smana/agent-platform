// SPDX-License-Identifier: Apache-2.0

package forge

import (
	"context"
	"errors"
	"time"
)

// ErrNoMerger is what every call on Absent answers: the merger App's key is not mounted.
var ErrNoMerger = errors.New("forge: the merger App is not configured (github.mergerAppIDFile): nothing can read checks or merge")

// Absent is the Merger of a factory started without the merger App's key, allowed only while
// no class is live or in shadow (config.NeedsMerger). It fails closed: a task that reaches the
// merge gate errors and retries, and no other identity reads checks or merges in its place.
type Absent struct{}

var _ Merger = Absent{}

// PullRequestChecks implements the Merger's PullRequestChecks and fails closed.
func (Absent) PullRequestChecks(context.Context, int) (Checks, error) { return Checks{}, ErrNoMerger }

// CommitChecks implements the Merger's CommitChecks and fails closed.
func (Absent) CommitChecks(context.Context, string) ([]Check, error) { return nil, ErrNoMerger }

// IsAncestor implements the Merger's IsAncestor and fails closed.
func (Absent) IsAncestor(context.Context, string, string) (bool, error) { return false, ErrNoMerger }

// RerunCheck implements Merger.
func (Absent) RerunCheck(context.Context, int64) error { return ErrNoMerger }

// WorkflowRuns implements the Merger's WorkflowRuns and fails closed.
func (Absent) WorkflowRuns(context.Context, string) ([]WorkflowRun, error) { return nil, ErrNoMerger }

// RunArtifact implements the Merger's RunArtifact and fails closed.
func (Absent) RunArtifact(context.Context, int64, string, string) ([]byte, error) {
	return nil, ErrNoMerger
}

// FileAt implements the Merger's FileAt and fails closed.
func (Absent) FileAt(context.Context, string, string) (string, error) { return "", ErrNoMerger }

// Diff implements the Merger's Diff and fails closed.
func (Absent) Diff(context.Context, string, string) (Diff, error) { return Diff{}, ErrNoMerger }

// Merge implements the Merger's Merge and fails closed.
func (Absent) Merge(context.Context, string, string) error { return ErrNoMerger }

// EnableAutoMerge implements the Merger's EnableAutoMerge and fails closed.
func (Absent) EnableAutoMerge(context.Context, string, string) error { return ErrNoMerger }

// DisableAutoMerge implements the Merger's DisableAutoMerge and fails closed.
func (Absent) DisableAutoMerge(context.Context, string) error { return ErrNoMerger }

// RevertPR implements the Merger's RevertPR and fails closed.
func (Absent) RevertPR(context.Context, string, string, string) (Revert, error) {
	return Revert{}, ErrNoMerger
}

// AddLabels implements the Merger's AddLabels and fails closed.
func (Absent) AddLabels(context.Context, int, ...string) error { return ErrNoMerger }

// PullsWithLabel implements the Merger's PullsWithLabel and fails closed.
func (Absent) PullsWithLabel(context.Context, string) ([]LabelledPull, error) {
	return nil, ErrNoMerger
}

// MergedSince implements the Merger's MergedSince and fails closed.
func (Absent) MergedSince(context.Context, time.Time) ([]MergedPull, error) { return nil, ErrNoMerger }

// PullFiles implements the Merger's PullFiles and fails closed.
func (Absent) PullFiles(context.Context, int) ([]FilePatch, error) { return nil, ErrNoMerger }

// BlameAtParent implements the Merger's BlameAtParent and fails closed.
func (Absent) BlameAtParent(context.Context, string, string) ([]BlameRange, error) {
	return nil, ErrNoMerger
}
