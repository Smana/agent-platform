// SPDX-License-Identifier: Apache-2.0

package decision

import (
	"strings"
	"testing"
)

func TestTheMarkerIsOneLine(t *testing.T) {
	got := Marker("abc", "docs-links", "shadow", "shadow_would_arm", Rows{Files: 1, MaxFiles: 3, Lines: 2, MaxLines: 20, Counted: true,
		Shape: "ok", Holdout: "n/a", Oracle: "n/a", Age: "n/a", Evidence: "ok", Reviewer: "glm-5.3"})
	want := "<!-- agent-factory-decision head=abc class=docs-links decision=shadow:shadow_would_arm files=1/3 lines=2/20 shape=ok holdout=n/a oracle=n/a age=n/a evidence=ok paused=false capped=false reviewer=glm-5.3 -->"
	if got != want {
		t.Fatalf("%q\nwant %q", got, want)
	}
}

// Ruling R-m7: a refusal leaves its later rows unread, and the marker prints them as "-", so
// every field stays one token a tally can split on. Counts of a diff not read are unread too:
// the caps print, never a count of 0 nobody took.
func TestAnUnreadRowIsADash(t *testing.T) {
	got := Marker("abc", "", "human", "head_unreported", Rows{MaxFiles: 3, MaxLines: 20, Reviewer: "none"})
	want := "<!-- agent-factory-decision head=abc class=- decision=human:head_unreported files=-/3 lines=-/20 shape=- holdout=- oracle=- age=- evidence=- paused=false capped=false reviewer=none -->"
	if got != want {
		t.Fatalf("%q\nwant %q", got, want)
	}
}

// A diff read with no change is a count of 0, not an unread one.
func TestACountedEmptyDiffIsZero(t *testing.T) {
	if got := Marker("abc", "c", "human", "r", Rows{Counted: true}); !strings.Contains(got, " files=0/0 lines=0/0 ") {
		t.Fatal(got)
	}
}
