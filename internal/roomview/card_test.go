// SPDX-License-Identifier: Apache-2.0

package roomview

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/policy"
	"github.com/Smana/agent-platform/internal/summary"
)

var cardGoldens = []string{"normal", "resumed", "expired_approval", "sealed", "no_facts", "runs_and_spend", "hostile"}

// foldGolden folds and views a summary golden exactly as summary's TestFoldGolden does.
func foldGolden(t *testing.T, name string) (summary.Summary, time.Time) {
	t.Helper()
	b, err := os.ReadFile("../summary/testdata/" + name + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var tc struct {
		Events []envelope.Event `json:"events"`
		Viewer struct {
			Role     string `json:"role"`
			Approver bool   `json:"approver"`
			Driver   bool   `json:"driver"`
			WebUI    bool   `json:"webUI"`
		} `json:"viewer"`
		Now time.Time `json:"now"`
	}
	if err := json.Unmarshal(b, &tc); err != nil {
		t.Fatal(err)
	}
	v := policy.Subject{Kind: envelope.ActorHuman, Role: policy.ParseRole(tc.Viewer.Role),
		Approver: tc.Viewer.Approver, Driver: tc.Viewer.Driver, WebUI: tc.Viewer.WebUI}
	return summary.View(summary.Fold(tc.Events), "26zfnuxm", rooms+"/r/26zfnuxm", v, 0, tc.Now), tc.Now
}

func card1(t *testing.T, s summary.Summary, o Options) string {
	t.Helper()
	var b bytes.Buffer
	if err := Card(&b, s, o); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

// body is a summary/v1 body decoded, as roomctl status decodes the broker's answer.
func body(t *testing.T, raw string) summary.Summary {
	t.Helper()
	var s summary.Summary
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func textOpts(n time.Time) Options { return Options{Format: Text, Now: n, Rooms: rooms} }

func TestCardGolden(t *testing.T) {
	for _, name := range cardGoldens {
		t.Run(name, func(t *testing.T) {
			s, n := foldGolden(t, name)
			md := card1(t, s, Options{Format: Markdown, Now: n, Rooms: rooms})
			golden(t, "card_"+name+".md", md)
			golden(t, "card_"+name+".txt", card1(t, s, Options{Format: Text, Now: n, Rooms: rooms}))
			if name != "hostile" {
				assertInert(t, md, rooms, nil)
			}
		})
	}
}

func TestCardMatchesTheSpec(t *testing.T) {
	s, n := foldGolden(t, "runs_and_spend")
	md := card1(t, s, Options{Format: Markdown, Now: n, Rooms: rooms})
	want := []string{
		"### 26zfnuxm · #42\n",
		"\n**needs you** · an agent is coding · Smana/cloud-native-ref · [open the room](https://rooms.example/r/26zfnuxm)\n",
		"\n| | |\n|---|---|\n| Run | implementer c3 (human review) · started 19:11 UTC, 18 min ago |\n",
		"| Spend | $0.31 of $1.50 (21 %) · 189 k tokens |\n",
		"| PR | [#2239](https://github.com/Smana/cloud-native-ref/pull/2239) · reviewer approved at 18:49 UTC |\n",
		"\n**Needs you**\n1. approve `git push to agent/26zfnuxm` by 19:45 UTC → [decide in the room](https://rooms.example/r/26zfnuxm#01M4A)\n",
		"\n**Runs** implementer (first run) succeeded · 40 min · $0.18 → reviewer approved · 10 min · $0.03 → implementer (human review) **running** · 18 min · $0.10\n",
		"\n**Agents' notes**, their claims, not checked\n- 19:12 UTC cf4ato2x `found both versions on line 12-13, fixing`\n",
		"\n**You can** queue a note for the next run · steer the running agent · stop it with the label `factory/stop`\n",
	}
	at := 0
	for _, w := range want {
		i := strings.Index(md[at:], w)
		if i < 0 {
			t.Fatalf("no %q after byte %d in\n%s", w, at, md)
		}
		at += i + len(w) - 1
	}
}

func TestCardRanOn(t *testing.T) {
	s, n := foldGolden(t, "runs_and_spend")
	s.Status.Run.Provenance = &envelope.ProvenanceFact{FactoryVersion: "v0.24.0", ServedModels: []string{"glm-5.3"}}
	md := card1(t, s, Options{Format: Markdown, Now: n, Rooms: rooms})
	if !strings.Contains(md, "\nRan on factory v0.24.0 · glm-5.3\n\n**You can**") {
		t.Errorf("no ran-on line before You can:\n%s", md)
	}
	if strings.Contains(card1(t, s, textOpts(n)), "`") {
		t.Error("a terminal card has no backticks")
	}
}

func TestCardOmitsAbsentLines(t *testing.T) {
	s, n := foldGolden(t, "no_facts")
	got := card1(t, s, Options{Format: Markdown, Now: n, Rooms: rooms})
	for _, w := range []string{"Run", "Spend", "PR", "Runs", "Needs you", "Agents'", "Ran on"} {
		if strings.Contains(got, w) {
			t.Errorf("no_facts has a %q line:\n%s", w, got)
		}
	}
	s, n = foldGolden(t, "sealed")
	got = card1(t, s, Options{Format: Markdown, Now: n, Rooms: rooms})
	for _, w := range []string{"Needs you", "You can", "| Run |"} {
		if strings.Contains(got, w) {
			t.Errorf("sealed has a %q line:\n%s", w, got)
		}
	}
	if !strings.Contains(got, "**Runs** implementer (first run) succeeded") || !strings.Contains(got, "| Spend | $0.18 of $1.50 (12 %) |") {
		t.Errorf("a sealed room keeps its runs and spend:\n%s", got)
	}
}

func TestCardNoIssue(t *testing.T) {
	s, n := foldGolden(t, "no_facts")
	for _, f := range []Format{Markdown, Text} {
		first := strings.SplitN(card1(t, s, Options{Format: f, Now: n, Rooms: rooms}), "\n", 2)[0]
		want := "### 26zfnuxm"
		if f == Text {
			want = "26zfnuxm"
		}
		if first != want {
			t.Errorf("format %d: heading %q, want %q", f, first, want)
		}
	}
}

func TestCardFallsBackToTokens(t *testing.T) {
	s := body(t, `{"room":"r","url":"`+rooms+`/r/r","status":{"phase":"Implementing","budget":{"usedTokens":182000,"limitTokens":1500000}},"notes":{"items":[]}}`)
	const want = "182 k of 1.5 M tokens (12 %)"
	if got := card1(t, s, textOpts(now)); !strings.Contains(got, want) {
		t.Errorf("no spend:\n%s", got)
	}
	// An unpriced run costs 0 (RunRecord.Unpriced): the card never reads $0.00 of $1.50.
	s.Status.Spend = &envelope.SpendFact{UsedMicroUSD: 0, LimitMicroUSD: 1_500_000}
	got := card1(t, s, textOpts(now))
	if !strings.Contains(got, want) || strings.Contains(got, "$") {
		t.Errorf("a spend of 0:\n%s", got)
	}
}

func TestCardSpendLine(t *testing.T) {
	for _, tc := range []struct {
		name        string
		used, limit int64
		micro, cap  int64
		want        string
	}{
		{"under a thousand", 999, 1_500_000, 0, 0, "999 of 1.5 M tokens (0 %)"},
		{"a thousand", 1000, 1_500_000, 0, 0, "1 k of 1.5 M tokens (0 %)"},
		{"just under the M rounding", 999_499, 1_500_000, 0, 0, "999 k of 1.5 M tokens (67 %)"},
		{"rounds up to a million", 999_500, 1_500_000, 0, 0, "1 M of 1.5 M tokens (67 %)"},
		{"a million", 1_000_000, 1_500_000, 0, 0, "1 M of 1.5 M tokens (67 %)"},
		{"a million and a half", 1_500_000, 1_500_000, 0, 0, "1.5 M of 1.5 M tokens (100 %)"},
		{"no limit, under a thousand", 999, 0, 0, 0, "999 tokens"},
		{"no limit, a thousand", 1000, 0, 0, 0, "1 k tokens"},
		{"no limit, 999 k", 999_499, 0, 0, 0, "999 k tokens"},
		{"no limit, 1 M", 999_500, 0, 0, 0, "1 M tokens"},
		{"no limit", 5000, 0, 0, 0, "5 k tokens"},
		{"dollars with a cap", 5000, 0, 310_000, 1_500_000, "$0.31 of $1.50 (21 %) · 5 k tokens"},
		{"dollars with no cap", 0, 0, 310_000, 0, "$0.31"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := summary.Summary{Room: "r"}
			if tc.used > 0 || tc.limit > 0 {
				s.Status.Budget = &envelope.BudgetFact{UsedTokens: tc.used, LimitTokens: tc.limit}
			}
			if tc.micro > 0 {
				s.Status.Spend = &envelope.SpendFact{UsedMicroUSD: tc.micro, LimitMicroUSD: tc.cap}
			}
			got := card1(t, s, textOpts(now))
			if !strings.Contains(got, "Spend "+tc.want+"\n") || strings.Contains(got, "/ 0") {
				t.Errorf("want Spend %q in\n%s", tc.want, got)
			}
		})
	}
}

func TestRunsLine(t *testing.T) {
	at := func(m int) time.Time { return now.Add(time.Duration(m-60) * time.Minute) } // minutes into the hour before now
	runs := []envelope.RunFact{
		{ID: "c1", Role: "implementer", Trigger: "initial", StartedAt: at(0), EndedAt: at(4), Outcome: "succeeded", MicroUSD: 80000},
		{ID: "c2", Role: "reviewer", Trigger: "review", StartedAt: at(5), EndedAt: at(8), Outcome: "succeeded", Verdict: "approve", MicroUSD: 40000},
		{ID: "c3", Role: "implementer", Trigger: "human", StartedAt: now.Add(-8 * time.Minute), MicroUSD: 80000},
	}
	s := summary.Summary{Room: "r", Runs: runs}
	got := card1(t, s, Options{Format: Markdown, Now: now, Rooms: rooms})
	const want = "**Runs** implementer (first run) succeeded · 4 min · $0.08 → reviewer approved · 3 min · $0.04 → implementer (human review) **running** · 8 min · $0.08\n"
	if !strings.Contains(got, want) {
		t.Errorf("want %q in\n%s", want, got)
	}

	// An outcome of ended (a stopped task's unfinished run) is never running, with or without an end.
	for _, ended := range []envelope.RunFact{
		{ID: "c4", Role: "implementer", Trigger: "retry", StartedAt: at(0), Outcome: "ended"},
		{ID: "c4", Role: "implementer", Trigger: "retry", StartedAt: at(0), EndedAt: at(2), Outcome: "ended"},
		{ID: "c4", Role: "implementer", Trigger: "retry", StartedAt: at(0), EndedAt: at(2)},
	} {
		s.Runs = []envelope.RunFact{ended}
		for _, f := range []Format{Markdown, Text, TextColor} {
			got := card1(t, s, Options{Format: f, Now: now, Rooms: rooms})
			if strings.Contains(got, "running") || !strings.Contains(got, "implementer (retry) ended") {
				t.Errorf("format %d, run %+v:\n%s", f, ended, got)
			}
		}
	}
	// A tester is a reviewer: no trigger, and its verdict in place of "succeeded"; a failed one says so.
	s.Runs = []envelope.RunFact{
		{ID: "t1", Role: "tester", Trigger: "ci", Outcome: "succeeded", Verdict: "changes", StartedAt: at(0), EndedAt: at(1)},
		{ID: "t2", Role: "reviewer", Outcome: "failed", Verdict: "approve", StartedAt: at(0), EndedAt: at(1)},
	}
	got = card1(t, s, textOpts(now))
	if !strings.Contains(got, "tester changes requested · 1 min → reviewer failed · 1 min\n") {
		t.Errorf("tester and failed reviewer:\n%s", got)
	}
}

func TestCardHostile(t *testing.T) {
	s, n := foldGolden(t, "hostile")
	md := card1(t, s, Options{Format: Markdown, Now: n, Rooms: rooms})
	untrusted := []string{"evil.example", "rm -rf", "# heading", "**bold**", "a|b", "onerror"}
	assertInert(t, md, rooms, untrusted)
	spans := strings.Join(codeSpans(md), "\n")
	for _, note := range s.Notes.Items {
		if !strings.Contains(spans, clean(note.Text)) {
			t.Errorf("note %q is not in a code span:\n%s", note.Text, md)
		}
	}
	if txt := card1(t, s, textOpts(n)); strings.ContainsAny(txt, "\x1b\u202e\u200b") {
		t.Errorf("hidden characters in\n%q", txt)
	}
}

func TestCardSize(t *testing.T) {
	for _, name := range cardGoldens {
		s, n := foldGolden(t, name)
		for _, f := range []Format{Markdown, Text} {
			if got := strings.Count(card1(t, s, Options{Format: f, Now: n, Rooms: rooms}), "\n"); got > 30 {
				t.Errorf("%s in format %d: %d lines", name, f, got)
			}
		}
	}
}

// The factory's phases are capitalised ("Implementing"), and the card reads them in words.
func TestCardRealPhases(t *testing.T) {
	for _, phase := range summary.TaskPhases() {
		s := summary.Summary{Room: "r", URL: rooms + "/r/r"}
		s.Status.Phase = phase
		got := strings.Split(card1(t, s, textOpts(now)), "\n")[1]
		state := summary.Coarse(phase, phase, false)
		if want := stateWord(state, false) + " · " + phaseWords(phase) + " · open the room (" + rooms + "/r/r)"; got != want {
			t.Errorf("%s: state line %q, want %q", phase, got, want)
		}
	}
}

// summaryBody is the broker's answer the roomctl tests use.
const summaryBody = `{"apiVersion":"summary/v1","room":"26zfnuxm","url":"https://rooms.example/r/26zfnuxm",` +
	`"status":{"phase":"Implementing","run":{"id":"cf4ato2x","role":"implementer"},"budget":{"usedTokens":189093,"limitTokens":1500000},` +
	`"pr":{"number":2239,"url":"https://github.com/Smana/a/pull/2239"},"issue":null,"lastVerdict":{"by":"reviewer","verdict":"changes_requested\u001b[31m","at":"2026-10-08T19:00:00Z"}},` +
	`"needsYou":[{"kind":"approval","id":"01M4","what":"git push to agent/26zfnuxm","deadline":"2026-10-08T14:00:00Z","url":"https://rooms.example/r/26zfnuxm#01M4"}],` +
	`"actions":[{"kind":"queue","what":"queue a message","cli":"roomctl post 26zfnuxm --queue <text>"},{"kind":"steer","what":"steer the run"}],` +
	`"notes":{"untrusted":true,"items":[{"at":"2026-10-08T19:14:00Z","run":"cf4ato2x","text":"found both versions\u001b[2J on line 12-13, fixing"}]},"cursor":"seq:142"}`

var cardNow = time.Date(2026, 10, 8, 19, 30, 0, 0, time.UTC)

// Ported from roomctl's RenderSummary tests: each case keeps its point on the card's own lines.
func TestCardPortedFromRenderSummary(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		now  time.Time
		has  []string
		not  []string
	}{
		{"escape and no approve command", summaryBody, cardNow,
			[]string{"needs you · an agent is coding · Smana/a · open the room (https://rooms.example/r/26zfnuxm)",
				`approve "git push to agent/26zfnuxm" by 14:00 UTC → https://rooms.example/r/26zfnuxm#01M4`,
				`19:14 UTC cf4ato2x "found both versions[2J on line 12-13, fixing"`,
				"PR    #2239 (https://github.com/Smana/a/pull/2239) · reviewer changes_requested[31m at 19:00 UTC"},
			[]string{"\x1b", "roomctl approve", "cursor"}},
		{"the task leads and the budget reads as the page does",
			`{"room":"r","url":"u","status":{"phase":"Implementing","run":{"id":"cf4ato2x","role":"implementer"},"budget":{"usedTokens":189093,"limitTokens":1500000},` +
				`"issue":{"number":74,"url":"https://github.com/o/r/issues/74","title":"Chat view: pair tool calls"}},"notes":{"items":[]}}`, cardNow,
			[]string{"r · #74 Chat view: pair tool calls\n", "Spend 189 k of 1.5 M tokens (13 %)\n", "o/r"}, nil},
		{"a queued room with an issue and no needs",
			`{"room":"26zfnuxm","url":"u","status":{"phase":"Queued","issue":{"number":42,"url":"https://github.com/Smana/a/issues/42"}},"needsYou":[],"actions":[],"notes":{"items":[]}}`, cardNow,
			[]string{"26zfnuxm · #42\nqueued · queued for a run · Smana/a\n"}, []string{"Needs you", "needs you"}},
		{"a hold is what releases it, with the room's link, and no approval",
			`{"room":"26zfnuxm","url":"https://rooms.example/r/26zfnuxm","status":{"phase":"Queued"},` +
				`"needsYou":[{"kind":"hold","id":"waiting_human_driver","what":"hand the driver back","url":"https://rooms.example/r/26zfnuxm"}],"notes":{"items":[]}}`, cardNow,
			[]string{"Needs you\n1. \"hand the driver back\" → https://rooms.example/r/26zfnuxm\n"}, []string{"approve"}},
		{"another day dates the deadline and the note", summaryBody, time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC),
			[]string{"by 2026-10-08 14:00 UTC →", "- 2026-10-08 19:14 UTC cf4ato2x"}, nil},
		{"why and next",
			`{"room":"r","url":"u","status":{"phase":"Escalated\u001b[2J","reason":"CI stayed red after the fix runs",` +
				"\"next\":\"a maintainer comments `/factory retry` on the issue\"," +
				`"issue":{"number":42,"url":"https://github.com/Smana/a/issues/42","title":"fix the CA"}},"notes":{"items":[]}}`, cardNow,
			[]string{"r · #42 fix the CA\n", "Escalated[2J", "Next  a maintainer comments `/factory retry` on the issue\n", "Why   CI stayed red after the fix runs\n"},
			[]string{"\x1b"}},
		{"a merge carries its PR link",
			`{"room":"r","url":"u","status":{"phase":"AwaitingHuman"},` +
				`"needsYou":[{"kind":"merge","id":"2267","what":"merge PR #2267: tick bypass","url":"https://github.com/Smana/a/pull/2267"}],"notes":{"items":[]}}`, cardNow,
			[]string{`1. "merge PR #2267: tick bypass" → https://github.com/Smana/a/pull/2267`}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := card1(t, body(t, tc.body), textOpts(tc.now))
			for _, w := range tc.has {
				if !strings.Contains(got, w) {
					t.Errorf("no %q in\n%s", w, got)
				}
			}
			for _, w := range tc.not {
				if strings.Contains(got, w) {
					t.Errorf("unwanted %q in\n%s", w, got)
				}
			}
		})
	}
}
