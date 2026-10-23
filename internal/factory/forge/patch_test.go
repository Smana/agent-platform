// SPDX-License-Identifier: Apache-2.0

package forge

import (
	"slices"
	"testing"
)

// BaseLines are the base-side line numbers a unified patch removes or rewrites: what a later pull
// request changed of the file as it found it (merge gate R35). Pure additions change none.
func TestBaseLines(t *testing.T) {
	patch := "@@ -3,4 +3,4 @@ name: x\n context\n-  version: 1.2.3\n+  version: 1.2.4\n context\n context\n" +
		"@@ -20,2 +20,3 @@\n context\n+added\n context\n@@ -40,3 +41,1 @@\n-gone a\n-gone b\n keep\n"
	if got := BaseLines(patch); !slices.Equal(got, []int{4, 40, 41}) {
		t.Fatalf("%v", got)
	}
	if BaseLines("") != nil || BaseLines("Binary files differ") != nil {
		t.Fatal("no hunk, no line")
	}
}

// A "\ No newline at end of file" marker is no line of either side, a header without a count is
// one line, and a line listed twice is listed once.
func TestBaseLinesSkipsMarkersAndDeduplicates(t *testing.T) {
	patch := "@@ -7 +7 @@\n-old\n\\ No newline at end of file\n+new\n\\ No newline at end of file\n@@ -7,1 +7,1 @@\n-old again\n"
	if got := BaseLines(patch); !slices.Equal(got, []int{7}) {
		t.Fatalf("%v", got)
	}
	if got := BaseLines("@@ -1,2 +1,2 @@\n+only\n+adds\n"); got != nil {
		t.Fatalf("pure additions change no base line: %v", got)
	}
}
