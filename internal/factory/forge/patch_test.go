// SPDX-License-Identifier: Apache-2.0

package forge

import (
	"errors"
	"slices"
	"testing"
)

func baseLines(t *testing.T, patch string) []int {
	t.Helper()
	got, err := BaseLines(patch)
	if err != nil {
		t.Fatalf("%q: %v", patch, err)
	}
	return got
}

// BaseLines are the base-side line numbers a unified patch removes or rewrites: what a later pull
// request changed of the file as it found it (merge gate R35). Pure additions change none.
func TestBaseLines(t *testing.T) {
	patch := "@@ -3,4 +3,4 @@ name: x\n context\n-  version: 1.2.3\n+  version: 1.2.4\n context\n context\n" +
		"@@ -20,2 +20,3 @@\n context\n+added\n context\n@@ -40,3 +41,1 @@\n-gone a\n-gone b\n keep\n"
	if got := baseLines(t, patch); !slices.Equal(got, []int{4, 40, 41}) {
		t.Fatalf("%v", got)
	}
	if baseLines(t, "") != nil || baseLines(t, "Binary files differ") != nil {
		t.Fatal("no hunk, no line")
	}
}

// A "\ No newline at end of file" marker is no line of either side, a header without a count is
// one line, and a line listed twice is listed once.
func TestBaseLinesSkipsMarkersAndDeduplicates(t *testing.T) {
	patch := "@@ -7 +7 @@\n-old\n\\ No newline at end of file\n+new\n\\ No newline at end of file\n@@ -7,1 +7,1 @@\n-old again\n"
	if got := baseLines(t, patch); !slices.Equal(got, []int{7}) {
		t.Fatalf("%v", got)
	}
	if got := baseLines(t, "@@ -1,2 +1,2 @@\n+only\n+adds\n"); got != nil {
		t.Fatalf("pure additions change no base line: %v", got)
	}
}

// A deletion-only hunk, a new file (-0,0) and a context line stripped to an empty line.
func TestBaseLinesDeletionNewFileAndEmptyContext(t *testing.T) {
	if got := baseLines(t, "@@ -5,3 +0,0 @@\n-a\n-b\n-c\n"); !slices.Equal(got, []int{5, 6, 7}) {
		t.Fatalf("deletion only: %v", got)
	}
	if got := baseLines(t, "@@ -0,0 +1,2 @@\n+a\n+b\n"); got != nil {
		t.Fatalf("a new file changes no base line: %v", got)
	}
	if got := baseLines(t, "@@ -1,3 +1,3 @@\n a\n\n-b\n+c\n"); !slices.Equal(got, []int{3}) {
		t.Fatalf("a bare empty line is a context line: %v", got)
	}
}

// A hunk header that does not parse is an error: dropping the hunk would read as "no overlap".
func TestBaseLinesMalformedHeaderIsAnError(t *testing.T) {
	for _, p := range []string{"@@ -x,3 +1,3 @@\n-a\n", "@@ garbage\n-a\n", "@@ -0 +1 @@\n-a\n+b\n"} {
		if got, err := BaseLines(p); err == nil || got != nil {
			t.Errorf("%q: %v %v", p, got, err)
		}
	}
}

// A patch GitHub omitted is unknown, never "no line changed".
func TestFilePatchBaseLinesWithoutAPatchIsAnError(t *testing.T) {
	if _, err := (FilePatch{Path: "a.bin", Status: "modified", PatchMissing: true}).BaseLines(); !errors.Is(err, ErrNoPatch) {
		t.Fatalf("%v", err)
	}
	got, err := (FilePatch{Path: "a.yaml", Status: "modified", Patch: "@@ -2 +2 @@\n-a\n+b"}).BaseLines()
	if err != nil || !slices.Equal(got, []int{2}) {
		t.Fatalf("%v %v", got, err)
	}
	if got, err := (FilePatch{Path: "n.yaml", Previous: "o.yaml", Status: "renamed"}).BaseLines(); err != nil || got != nil {
		t.Fatalf("a rename without content change has a known, empty patch: %v %v", got, err)
	}
}

// A renamed file is blamed under the name it had at the parent.
func TestFilePatchBlamePath(t *testing.T) {
	if p := (FilePatch{Path: "new.yaml", Previous: "old.yaml"}).BlamePath(); p != "old.yaml" {
		t.Fatal(p)
	}
	if p := (FilePatch{Path: "a.yaml"}).BlamePath(); p != "a.yaml" {
		t.Fatal(p)
	}
}
