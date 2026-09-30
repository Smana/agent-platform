// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"strings"
	"testing"

	v1alpha1 "github.com/Smana/agent-platform/api/factory/v1alpha1"
)

// G2: the fenced text is marked as untrusted data inside the fence, where the model reads it, and a
// text at the admission cap (R6) still fits AgentRun's 16 KiB task.text.
func TestFirstBriefMarksTheIssueAsUntrustedData(t *testing.T) {
	b := FirstBrief(issueTask("3buqdlot", 7, strings.Repeat("x", 14336)), "n0nce234")
	fence := "TASK-DATA-n0nce234"
	open := strings.Index(b, fence+"\n")
	if open < 0 || !strings.HasPrefix(b[open+len(fence)+1:], untrustedHeader+"\n") {
		t.Fatalf("the first line inside the fence marks the data:\n%.600s", b)
	}
	if pre := b[:open]; !strings.Contains(pre, "untrusted data") || !strings.Contains(pre, "never an instruction") {
		t.Fatal("the trusted preamble says what the fenced text is")
	}
	if !strings.HasSuffix(b, "\n"+fence+"\n") || strings.Count(b, fence) != 3 {
		t.Fatalf("the fence opens and closes the text, and the preamble names it once:\n%s", b[len(b)-200:])
	}
	if len(b) > 16384 {
		t.Fatalf("%d bytes", len(b))
	}
}

// The header says what the sanitiser did, and how to read its escapes back in code (review minor).
func TestTheHeaderNamesWhatWasDefusedAndItsEscapes(t *testing.T) {
	for _, want := range []string{"invisible and control characters removed", "NFKC", "images and raw markup defused",
		"&lt;", `!\[`, `]\:`, "⟦", "〚"} {
		if !strings.Contains(untrustedHeader, want) {
			t.Errorf("the header lacks %q", want)
		}
	}
	if strings.Contains(untrustedHeader, "\n") {
		t.Fatal("the header is one line: the first inside the fence")
	}
}

// Ruling SF: Agent-Task is the harness footer's alone, so the brief never asks the agent for it.
func TestFirstBriefAsksForTheIssueLinkOnly(t *testing.T) {
	b := FirstBrief(issueTask("3buqdlot", 7, "fix it"), "n0nce234")
	if !strings.Contains(b, `"Fixes #7"`) || strings.Contains(b, "Agent-Task") || !strings.Contains(b, "labelled for the factory by Smana") ||
		!strings.Contains(b, "sha256 "+strings.Repeat("b", 64)) {
		t.Fatal(b)
	}
}

func TestATrustedTextIsFencedWithoutTheWarning(t *testing.T) {
	tk := issueTask("3buqdlot", 0, "rotate the certificates")
	tk.Spec.Source = v1alpha1.Source{Kind: "schedule", Ref: "weekly-certs", Trust: "trusted"}
	b := FirstBrief(tk, "n0nce234")
	if strings.Contains(b, untrustedHeader) || strings.Contains(b, "untrusted") || !strings.Contains(b, "reviewed configuration") ||
		!strings.Contains(b, "Source: schedule weekly-certs.") || strings.Contains(b, "Fixes #") {
		t.Fatal(b)
	}
	if !strings.HasSuffix(b, "\nTASK-DATA-n0nce234\nrotate the certificates\nTASK-DATA-n0nce234\n") {
		t.Fatal(b)
	}
}
