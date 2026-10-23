// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"maps"
	"slices"
	"strings"
	"testing"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
	"github.com/Smana/agent-platform/internal/factory/config"
)

// steered gives tk the design's maximum note: 2048 bytes, valid.
func steered(tk *v1alpha1.Task) *v1alpha1.Task {
	text := "## Verify\n- " + strings.Repeat("v", 2048-len("## Verify\n- \n")) + "\n"
	tk.Spec.ScopeMode = "steering"
	tk.Spec.Steering = &v1alpha1.Steering{Path: ".github/agent-factory/steering/review.md", SHA: strings.Repeat("5", 40), Text: text}
	return tk
}

// "Briefs per mode", off: today's brief, byte for byte.
func TestOffBriefsAreTodays(t *testing.T) {
	tk := issueTask("3buqdlot", 7, "# Fix the link")
	off := *tk
	off.Spec.ScopeMode = "off"
	if FirstBrief(&off, "n0nce234") != FirstBrief(tk, "n0nce234") || strings.Contains(FirstBrief(&off, "n0nce234"), "STEERING-") {
		t.Fatal("off changed the first brief")
	}
}

// steering: preamble · steering · snapshot; the note is fenced as trusted and whole.
func TestTheFirstBriefCarriesTheNote(t *testing.T) {
	tk := steered(issueTask("3buqdlot", 7, "# Fix the link"))
	b := FirstBrief(tk, "n0nce234")
	note, ok := fenced(b, "STEERING-n0nce234")
	if !ok || note != strings.TrimRight(tk.Spec.Steering.Text, "\n") {
		t.Fatalf("the note is not fenced whole:\n%s", b)
	}
	if strings.Index(b, "STEERING-n0nce234") > strings.Index(b, "TASK-DATA-n0nce234") {
		t.Fatal("the note comes before the snapshot")
	}
	if !strings.Contains(b, tk.Spec.Steering.SHA) || !strings.Contains(b, "Fixes #7") {
		t.Fatalf("the preamble keeps its lines and names the blob:\n%s", b)
	}
}

// Offline proof 5: a 14 KiB snapshot and a 2 KiB note stay within 15 KiB through the snapshot pointer.
func TestABigSnapshotIsReadFromTheRoom(t *testing.T) {
	tk := steered(issueTask("3buqdlot", 7, strings.Repeat("s", 14336)))
	b := FirstBrief(tk, "n0nce234")
	if len(b) > firstCap || strings.Contains(b, strings.Repeat("s", 100)) || !strings.Contains(b, "sinceSeq 0") {
		t.Fatalf("%d bytes, snapshot inlined %v", len(b), strings.Contains(b, strings.Repeat("s", 100)))
	}
	if _, ok := fenced(b, "STEERING-n0nce234"); !ok {
		t.Fatal("the note is never moved")
	}
	if len(ResumeNotice(tk))+len(b) > 16<<10 {
		t.Fatal("a resumed run's brief must fit AgentRun's task.text")
	}
}

// Revisions: steering before the room log, within 13 KiB however full the log.
func TestARevisionCarriesTheNote(t *testing.T) {
	tk := steered(reviseTask())
	b, _ := ReviseBrief(tk, worstLog(), nil, "n0nce234")
	if len(b) > reviseCap {
		t.Fatalf("%d bytes", len(b))
	}
	s, r := strings.Index(b, "STEERING-n0nce234"), strings.Index(b, "ROOM-DATA-n0nce234")
	if s < 0 || r < 0 || s > r {
		t.Fatalf("steering at %d, the room log at %d", s, r)
	}
}

// SC3: the note is posted once in the room, after the snapshot, and reaches the first run.
func TestTheNoteIsPostedOnceAfterTheSnapshot(t *testing.T) {
	g := scopeRig(t, config.ScopeSteering)
	g.f.SetFile(".github/agent-factory/steering/review.md", reviewNote)
	g.reconcile(t, "3buqdlot", 3)
	g.reconcile(t, "3buqdlot", 2)
	seqs := slices.Sorted(maps.Keys(g.log.states)) // the room seq ledger is shared with the task's facts
	if len(seqs) != 2 || !strings.Contains(g.log.states[seqs[0]], "TASK-DATA-") || !strings.Contains(g.log.states[seqs[1]], reviewNote) {
		t.Fatalf("room states %v", g.log.states)
	}
	if !strings.Contains(g.runs.specs[rid(0)].TaskText, reviewNote) {
		t.Fatal("the first run's brief lacks the note")
	}
}
