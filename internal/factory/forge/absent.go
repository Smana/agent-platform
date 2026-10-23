// SPDX-License-Identifier: Apache-2.0

package forge

import (
	"context"
	"errors"
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

// RerunCheck implements Merger.
func (Absent) RerunCheck(context.Context, int64) error { return ErrNoMerger }

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
