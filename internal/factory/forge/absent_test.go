// SPDX-License-Identifier: Apache-2.0

package forge

import (
	"errors"
	"testing"
	"time"
)

// Every dead-links read on a factory without the merger App answers ErrNoMerger: no findings,
// never an empty success.
func TestAbsentFailsTheDeadLinksReadsClosed(t *testing.T) {
	var a Absent
	if _, err := a.WorkflowRuns(t.Context(), "dead-links.yaml"); !errors.Is(err, ErrNoMerger) {
		t.Errorf("WorkflowRuns: %v", err)
	}
	if _, err := a.RunArtifact(t.Context(), 1, "findings", "findings.json"); !errors.Is(err, ErrNoMerger) {
		t.Errorf("RunArtifact: %v", err)
	}
	if _, err := a.FileAt(t.Context(), "docs/a.md", "main"); !errors.Is(err, ErrNoMerger) {
		t.Errorf("FileAt: %v", err)
	}
}

// The follow-up reads on a factory without the merger App answer ErrNoMerger too: a missing
// answer is never "no merged pull request, no overlap".
func TestAbsentFailsTheFollowUpReadsClosed(t *testing.T) {
	var a Absent
	if ps, err := a.MergedSince(t.Context(), time.Time{}); !errors.Is(err, ErrNoMerger) || ps != nil {
		t.Errorf("MergedSince: %v", err)
	}
	if fs, err := a.PullFiles(t.Context(), 1); !errors.Is(err, ErrNoMerger) || fs != nil {
		t.Errorf("PullFiles: %v", err)
	}
	if rs, err := a.BlameAtParent(t.Context(), "sha", "a.yaml"); !errors.Is(err, ErrNoMerger) || rs != nil {
		t.Errorf("BlameAtParent: %v", err)
	}
}
