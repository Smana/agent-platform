// SPDX-License-Identifier: Apache-2.0

package roomctl

import (
	"strings"
	"testing"
	"time"
)

func render(t *testing.T, body string) string {
	t.Helper()
	var out strings.Builder
	if err := RenderSummary(&out, []byte(body), time.Now()); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// #59: a waiting phase prints why and what comes next; #50: the issue's title is printed.
func TestRenderSummaryPrintsTheReasonAndNextStep(t *testing.T) {
	got := render(t, `{"apiVersion":"summary/v1","room":"r","url":"u","status":{"phase":"Escalated\u001b[2J",`+
		`"reason":"CI stayed red after the fix runs","next":"a maintainer comments `+"`/factory retry`"+` on the issue",`+
		`"issue":{"number":42,"url":"https://github.com/Smana/a/issues/42","title":"fix the CA"}},`+
		`"needsYou":[],"actions":[],"notes":{"untrusted":true,"items":[]},"cursor":"seq:3"}`)
	want := "phase: Escalated[2J\n" +
		"issue #42 https://github.com/Smana/a/issues/42 \"fix the CA\"\n" +
		"why: CI stayed red after the fix runs\n" +
		"next: a maintainer comments `/factory retry` on the issue\n" +
		"needs you: none\ncursor: seq:3\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}

// #51: a merge is printed as the instruction it carries, with the PR's link.
func TestRenderSummaryPrintsAMerge(t *testing.T) {
	got := render(t, `{"apiVersion":"summary/v1","room":"r","url":"u","status":{"phase":"AwaitingHuman"},`+
		`"needsYou":[{"kind":"merge","id":"2267","what":"merge PR #2267: tick bypass","url":"https://github.com/Smana/a/pull/2267"}],`+
		`"actions":[],"notes":{"untrusted":true,"items":[]},"cursor":"seq:3"}`)
	want := "phase: AwaitingHuman\nneeds you: merge PR #2267: tick bypass → https://github.com/Smana/a/pull/2267\ncursor: seq:3\n"
	if got != want {
		t.Fatalf("got:\n%s\nwant:\n%s", got, want)
	}
}
