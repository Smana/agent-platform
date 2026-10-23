// SPDX-License-Identifier: Apache-2.0

package forge

import (
	"errors"
	"testing"
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
