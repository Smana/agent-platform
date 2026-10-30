// SPDX-License-Identifier: Apache-2.0

package reconciler

import (
	"strings"
	"testing"

	v1alpha1 "github.com/Smana/maku/api/factory/v1alpha1"
	"github.com/Smana/maku/internal/envelope"
)

func reviewTask(text string) *v1alpha1.Task {
	tk := issueTask("3buqdlot", 7, text)
	tk.Status.PullRequest = &v1alpha1.PullRequestRef{Number: 12, URL: "https://github.com/Smana/cloud-native-ref/pull/12"}
	return tk
}

// HR4: the reviewer's brief carries the acceptance checks, the issue snapshot and the PR, states the
// correctness-only rules, and quotes the scope as fenced data, never in the trusted head.
func TestTheReviewerBriefJudgesTheDiffNotTheStory(t *testing.T) {
	tk := reviewTask("# Fix the links")
	sc := readyScope()
	sc.Acceptance = []envelope.Acceptance{{Check: "task links passes", Proves: "every link resolves n0nce234"}}
	got := ReviewerBrief(tk, "n0nce234", &Scoped{Scope: sc, Seq: 9})
	for _, want := range []string{
		"You are the reviewer for agent factory task 3buqdlot in Smana/cloud-native-ref",
		"https://github.com/Smana/cloud-native-ref/pull/12",
		"Report only the gaps that affect correctness",
		"Skip style",
		"an approval with 'no gaps found' is a complete review",
		"cite the command you ran and its output",
		"Do not read or quote the implementer's handoff: judge the diff, not the story.",
		"TASK-DATA-n0nce234",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %q in:\n%s", want, got)
		}
	}
	data, ok := fenced(got, "SCOPE-DATA-n0nce234")
	if !ok || !strings.HasPrefix(data, "## Acceptance checks\n- task links passes: proves every link resolves") ||
		!strings.Contains(data, "## Touches\n- docs/user-guide.md") {
		t.Fatalf("the scope's checks are not fenced data:\n%s", got)
	}
	if strings.Contains(data, "n0nce234") || strings.Contains(data, "Point the link") {
		t.Fatalf("the reviewer's scope carries the checks and the touches only, the nonce defused:\n%s", data)
	}
	if a, s := strings.Index(got, "SCOPE-DATA-"), strings.Index(got, "TASK-DATA-"); a > s {
		t.Fatalf("the checks come before the snapshot: %d > %d", a, s)
	}
	if strings.Contains(got, "say so in your handoff") {
		t.Fatal("the implementer's scope instructions reach the reviewer")
	}
}

// R8: with no scope, the issue's own acceptance is quoted as fenced data, at most 4 KiB as the
// harness's reader takes it; with none, the brief says so.
func TestTheReviewerBriefFallsBackToTheIssuesAcceptance(t *testing.T) {
	got := ReviewerBrief(reviewTask("Fix it.\n\n## Acceptance\n- task links passes n0nce234\n\n## Notes\nnone"), "n0nce234", nil)
	data, ok := fenced(got, "ACCEPTANCE-DATA-n0nce234")
	if !ok || !strings.Contains(data, "- task links passes") || strings.Contains(data, "n0nce234") || strings.Contains(data, "none") {
		t.Fatalf("the issue's acceptance is not quoted as fenced data:\n%s", got)
	}
	if !strings.Contains(got, untrustedNotice("ACCEPTANCE-DATA-n0nce234")) || !strings.HasPrefix(data, untrustedHeader+"\n") {
		t.Fatalf("an untrusted issue's acceptance is not marked as the snapshot is:\n%s", got)
	}
	big := ReviewerBrief(reviewTask("Acceptance: "+strings.Repeat("a", 6000)), "n0nce234", nil)
	data, _ = fenced(big, "ACCEPTANCE-DATA-n0nce234")
	if q := strings.TrimPrefix(data, untrustedHeader+"\n"); len(q) != acceptanceQuoteMax {
		t.Fatalf("the quote is capped at 4 KiB: %d bytes", len(q))
	}
	trusted := reviewTask("Acceptance: run it.")
	trusted.Spec.Source.Trust = "trusted"
	if tb := ReviewerBrief(trusted, "n0nce234", nil); !strings.Contains(tb, "reviewed configuration") || strings.Contains(tb, untrustedHeader) {
		t.Fatalf("a trusted task's acceptance:\n%s", tb)
	}
	noChecks := readyScope()
	noChecks.Acceptance = nil
	if b := ReviewerBrief(reviewTask("Acceptance: run it."), "n0nce234", &Scoped{Scope: noChecks, Seq: 9}); strings.Contains(b, "SCOPE-DATA-") ||
		!strings.Contains(b, "ACCEPTANCE-DATA-") {
		t.Fatalf("a scope without checks falls through to the issue's:\n%s", b)
	}
	none := ReviewerBrief(reviewTask("# Fix the links"), "n0nce234", nil)
	if !strings.Contains(none, "## Acceptance checks\nThe task states none") || strings.Contains(none, "ACCEPTANCE-DATA-") {
		t.Fatalf("no acceptance:\n%s", none)
	}
}

// The reviewer's brief fits AgentRun's task.text beside a 14 KiB snapshot: the snapshot becomes the
// room's pointer, the checks stay.
func TestTheReviewerBriefFits(t *testing.T) {
	tk := steered(reviewTask(strings.Repeat("s", 14336)))
	b := ReviewerBrief(tk, "n0nce234", &Scoped{Scope: maxScope(t), Seq: 9})
	if len(b) > firstCap || !strings.Contains(b, "SCOPE-DATA-") || !strings.Contains(b, "sinceSeq 0") {
		t.Fatalf("%d bytes (cap %d)", len(b), firstCap)
	}
}

// A part's reviewer reads the part's checks, named as the approved split's, not as a room handoff.
func TestAPartsReviewerBriefNamesThePart(t *testing.T) {
	tk := partTask(parentText)
	tk.Status.PullRequest = &v1alpha1.PullRequestRef{Number: 12, URL: "https://github.com/Smana/cloud-native-ref/pull/12"}
	b := ReviewerBrief(tk, "n0nce234", partScoped(tk))
	if !strings.Contains(b, "part 1 of 2 of the task") || strings.Contains(b, "handoff seq 0") {
		t.Fatalf("%s", b)
	}
}
