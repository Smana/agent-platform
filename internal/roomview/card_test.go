// SPDX-License-Identifier: Apache-2.0

package roomview

import (
	"bytes"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/yuin/goldmark/ast"

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
		"### #42 · 26zfnuxm\n",
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
	// A tester writes the tests (TO1): shown as a writer, with its trigger and its outcome, not a verdict; a reviewer
	// shows its verdict in place of "succeeded", and a failed one says so.
	s.Runs = []envelope.RunFact{
		{ID: "t1", Role: "tester", Trigger: "ci", Outcome: "succeeded", Verdict: "changes", StartedAt: at(0), EndedAt: at(1)},
		{ID: "t2", Role: "reviewer", Outcome: "failed", Verdict: "approve", StartedAt: at(0), EndedAt: at(1)},
	}
	got = card1(t, s, textOpts(now))
	if !strings.Contains(got, "tester (CI fix) succeeded · 1 min → reviewer failed · 1 min\n") {
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
			[]string{"#74 Chat view: pair tool calls · r\n", "Spend 189 k of 1.5 M tokens (13 %)\n", "o/r"}, nil},
		{"a queued room with an issue and no needs",
			`{"room":"26zfnuxm","url":"u","status":{"phase":"Queued","issue":{"number":42,"url":"https://github.com/Smana/a/issues/42"}},"needsYou":[],"actions":[],"notes":{"items":[]}}`, cardNow,
			[]string{"#42 · 26zfnuxm\nqueued · queued for a run · Smana/a\n"}, []string{"Needs you", "needs you"}},
		{"a hold is what releases it, with the room's link, and no approval",
			`{"room":"26zfnuxm","url":"https://rooms.example/r/26zfnuxm","status":{"phase":"Queued"},` +
				`"needsYou":[{"kind":"hold","id":"waiting_human_driver","what":"hand the driver back","url":"https://rooms.example/r/26zfnuxm"}],"notes":{"items":[]}}`, cardNow,
			[]string{"Needs you\n1. hand the driver back → https://rooms.example/r/26zfnuxm\n"}, []string{"approve"}},
		{"another day dates the deadline and the note", summaryBody, time.Date(2026, 10, 9, 8, 0, 0, 0, time.UTC),
			[]string{"by 2026-10-08 14:00 UTC →", "- 2026-10-08 19:14 UTC cf4ato2x"}, nil},
		{"why and next",
			`{"room":"r","url":"u","status":{"phase":"Escalated\u001b[2J","reason":"CI stayed red after the fix runs",` +
				"\"next\":\"a maintainer comments `/factory retry` on the issue\"," +
				`"issue":{"number":42,"url":"https://github.com/Smana/a/issues/42","title":"fix the CA"}},"notes":{"items":[]}}`, cardNow,
			[]string{"#42 fix the CA · r\n", "Escalated[2J", "Next  a maintainer comments /factory retry on the issue\n", "Why   CI stayed red after the fix runs\n"},
			[]string{"\x1b"}},
		{"a merge carries its PR link",
			`{"room":"r","url":"u","status":{"phase":"AwaitingHuman"},` +
				`"needsYou":[{"kind":"merge","id":"2267","what":"merge PR #2267: tick bypass","url":"https://github.com/Smana/a/pull/2267"}],"notes":{"items":[]}}`, cardNow,
			[]string{`1. a maintainer with admin on Smana/a merges PR #2267: the ruleset blocks a plain merge, so tick "bypass rules" on the PR page, or run gh pr merge 2267 --squash --admin --repo Smana/a → https://github.com/Smana/a/pull/2267`}, nil},
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

// A served model or a version the factory writes is as inert as any other word.
func TestCardTokenAutolink(t *testing.T) {
	c := &card{o: Options{Format: Markdown, Rooms: rooms}}
	for _, w := range []string{"www.x.y/z", "a-www.x"} {
		if got := c.token(w, false); got != "`"+w+"`" {
			t.Errorf("token(%q) = %q, want a code span", w, got)
		}
	}
	if got := c.token("glm-5.3", false); got != "glm-5.3" {
		t.Errorf("token(glm-5.3) = %q", got)
	}
}

// Only untrusted text is cut: the broker's own instructions arrive whole, and their code spans
// are not wrapped in a second one.
func TestCardKeepsTheBrokersTextWhole(t *testing.T) {
	merge := summary.MergeHow("Smana/a", 2239)
	answer := "@alice answers the scoper's question by editing issue #42, then a maintainer re-applies `factory/ready`; or a maintainer comments `/factory retry` to proceed on its assumption"
	next := "nothing to do: the task goes on when the class's holdout checks report; a check that never reports keeps it waiting until merge.awaitingCIFor, if set (agent-platform #182)"
	for _, s := range []string{merge, answer, next} {
		if len([]rune(s)) <= maxText {
			t.Fatalf("the fixture must outrun the cut: %q", s)
		}
	}
	sum := summary.Summary{Room: "r", URL: rooms + "/r/r"}
	sum.Status.Phase = "AwaitingHuman"
	sum.Status.Next = next
	sum.NeedsYou = []summary.Need{
		{Kind: "merge", ID: "2239", What: merge, URL: "https://github.com/Smana/a/pull/2239"},
		{Kind: "answer", ID: "42", What: answer, URL: "https://github.com/Smana/a/issues/42"},
	}
	// The merge and answer lines are built from their parts, with the repository, the author and
	// the commands in code spans; a terminal shows the same words without the backticks.
	mergeMD := "a maintainer with admin on `Smana/a` merges PR #2239: the ruleset blocks a plain merge, so tick \"bypass rules\" on the PR page, or run `gh pr merge 2239 --squash --admin --repo Smana/a`"
	answerMD := "`@alice` answers the scoper's question by editing issue #42, then a maintainer re-applies `factory/ready`; or a maintainer comments `/factory retry` to proceed on its assumption"
	sum.NeedsYou[1].Author = "alice"
	for _, f := range []Format{Markdown, Text} {
		got := card1(t, sum, Options{Format: f, Now: now, Rooms: rooms})
		for _, s := range []string{mergeMD, answerMD, next} {
			want := s
			if f == Text && s != next {
				want = strings.ReplaceAll(s, "`", "")
			}
			if !strings.Contains(got, want) {
				t.Errorf("format %d: %q is cut or rewritten in\n%s", f, s, got)
			}
		}
		if strings.Contains(got, "…") {
			t.Errorf("format %d: a cut in\n%s", f, got)
		}
	}
	md := card1(t, sum, Options{Format: Markdown, Now: now, Rooms: rooms})
	assertInert(t, md, rooms, nil)
	if spans := strings.Join(codeSpans(md), "\n"); !strings.Contains(spans, "gh pr merge 2239 --squash --admin --repo Smana/a") || !strings.Contains(spans, "Smana/a\n") || strings.Contains(spans, "a maintainer with admin") {
		t.Errorf("the merge command is its own code span, not nested in another:\n%v", codeSpans(md))
	}
	// An approval's text is the agent's: cut.
	sum.NeedsYou = []summary.Need{{Kind: "approval", What: strings.Repeat("x", 300), URL: rooms + "/r/r#a"}}
	if got := card1(t, sum, textOpts(now)); strings.Contains(got, strings.Repeat("x", 161)) || !strings.Contains(got, "…") {
		t.Errorf("an approval's text is cut at %d runes:\n%s", maxText, got)
	}
}

// The Run row reads what PR 2's facts say about the current run: an ended run is ended, with when.
func TestCardRunRowReadsAnEndedRun(t *testing.T) {
	s, n := foldGolden(t, "runs_and_spend")
	s.Status.State = summary.StateDone
	s.Status.Phase = "Done"
	r := *s.Status.Run
	r.EndedAt, r.Outcome = r.StartedAt.Add(29*time.Minute), "succeeded"
	s.Status.Run = &r
	got := card1(t, s, Options{Format: Markdown, Now: n.Add(3 * time.Hour), Rooms: rooms})
	if !strings.Contains(got, "| Run | implementer c3 (human review) · started 19:11 UTC, ended 19:40 UTC (succeeded) |\n") || strings.Contains(got, " ago") {
		t.Errorf("an ended run:\n%s", got)
	}
}

// A run with no outcome on a task that is over reads ended, never running, in the row and the line.
func TestCardUnfinishedRunOfATerminalTaskReadsEnded(t *testing.T) {
	for _, state := range []string{summary.StateDone, summary.StateFailed, summary.StateStopped} {
		s, n := foldGolden(t, "runs_and_spend")
		s.Status.State = state
		for _, f := range []Format{Markdown, Text, TextColor} {
			got := card1(t, s, Options{Format: f, Now: n.Add(5 * time.Hour), Rooms: rooms})
			if strings.Contains(got, "(human review) running") || strings.Contains(got, "**running**") || strings.Contains(got, "ago") || !strings.Contains(got, "(human review) ended") {
				t.Errorf("%s, format %d:\n%s", state, f, got)
			}
		}
	}
}

// Reason, Next and a verdict's words are hostile here: they stay inert, on their own lines.
func TestCardHostileFields(t *testing.T) {
	h := "x`**[a](https://evil.example) www.evil.example a@b.example <b>\u202e\n# heading\n| a | b |"
	s := summary.Summary{Room: "26zfnuxm", URL: rooms + "/r/26zfnuxm"}
	s.Status.Phase = "Escalated"
	s.Status.Reason = h
	// Next is the broker's own sentence (summary.nextStep), with code spans of its own, so its markup
	// is not escaped: it only has to stay on its line and in its cell.
	s.Status.Next = "a maintainer comments `/factory retry`\n# heading\n| a | b |\u202e"
	s.Status.LastVerdict = &summary.Verdict{By: h, Verdict: h, At: now}
	s.Status.PR = &envelope.PRFact{Number: 7, URL: "https://github.com/Smana/a/pull/7"}
	s.Runs = []envelope.RunFact{{ID: h, Role: "reviewer", Verdict: h, Outcome: "succeeded", StartedAt: now, EndedAt: now.Add(time.Minute)}}
	md := card1(t, s, Options{Format: Markdown, Now: now, Rooms: rooms})
	golden(t, "card_hostile_fields.md", md)
	golden(t, "card_hostile_fields.txt", card1(t, s, textOpts(now)))
	// Reason and the verdict are the agents' or the factory's text: escaped.
	assertInert(t, md, rooms, []string{"evil.example", "a@b.example"})
	if strings.Contains(md, "\n# heading") || strings.Count(md, "\n| Next |") != 1 {
		t.Errorf("a field broke out of its line:\n%s", md)
	}
	if got := tableCells(t, md); len(got) != 1 {
		t.Errorf("tables: %v", got)
	} else {
		for i, n := range got[0] {
			if n != 2 {
				t.Errorf("row %d has %d cells:\n%s", i, n, md)
			}
		}
	}
}

func TestCardTextColorBoldsTheStateWord(t *testing.T) {
	s, n := foldGolden(t, "normal")
	got := card1(t, s, Options{Format: TextColor, Now: n, Rooms: rooms})
	if !strings.HasPrefix(strings.SplitN(got, "\n", 3)[1], "\x1b[1mneeds you\x1b[0m · ") {
		t.Errorf("the state word is bold on a terminal:\n%q", got)
	}
	if plain := card1(t, s, textOpts(n)); strings.Contains(plain, "\x1b") {
		t.Errorf("Text has an escape: %q", plain)
	}
}

// noEmphasis fails when the numbered need lines of md render emphasis (the state word is bold on
// purpose, so only those lines are read). Links are assertInert's: any off the allowlist fails.
func noEmphasis(t *testing.T, md string) {
	t.Helper()
	var lines []string
	for _, l := range strings.Split(md, "\n") {
		if strings.HasPrefix(l, "1. ") {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		t.Fatalf("no need line in\n%s", md)
	}
	root, _ := parse(strings.Join(lines, "\n"))
	_ = ast.Walk(root, func(n ast.Node, enter bool) (ast.WalkStatus, error) {
		if _, ok := n.(*ast.Emphasis); ok && enter {
			t.Errorf("emphasis renders from\n%s", strings.Join(lines, "\n"))
		}
		return ast.WalkContinue, nil
	})
}

// The merge need names a repository GitHub's naming rules let start with "_" and hold ".": the
// card builds the line from its parts, so neither a link nor emphasis can render.
func TestCardMergeNeedIsInertForAHostileRepository(t *testing.T) {
	for _, repo := range []string{"o/_www.evil.example", "o/_x_", "o/a.b"} {
		sum := summary.Summary{Room: "r", URL: rooms + "/r/r"}
		sum.Status.Phase = "AwaitingHuman"
		sum.NeedsYou = []summary.Need{{Kind: "merge", ID: "1", What: summary.MergeHow(repo, 1), URL: "https://github.com/" + repo + "/pull/1"}}
		md := card1(t, sum, Options{Format: Markdown, Now: now, Rooms: rooms})
		assertInert(t, md, rooms, nil)
		noEmphasis(t, md)
		spans := codeSpans(md)
		if !slices.Contains(spans, repo) || !slices.Contains(spans, "gh pr merge 1 --squash --admin --repo "+repo) {
			t.Errorf("%s: the repository and the command are code spans, got %q in\n%s", repo, spans, md)
		}
		txt := card1(t, sum, textOpts(now))
		if strings.Contains(txt, "`") || !strings.Contains(txt, "admin on "+repo+" merges PR #1") {
			t.Errorf("%s: terminal:\n%s", repo, txt)
		}
	}
	// With no repository the line says so and offers no gh command.
	sum := summary.Summary{Room: "r", URL: rooms + "/r/r"}
	sum.NeedsYou = []summary.Need{{Kind: "merge", ID: "1", What: summary.MergeHow("", 1), URL: "https://evil.example/x"}}
	if got := card1(t, sum, textOpts(now)); strings.Contains(got, "gh pr merge") || !strings.Contains(got, "bypass rules") {
		t.Errorf("no repository:\n%s", got)
	}
}

// The broker's command is never cut: a 39-character owner and a 100-character repository name
// (GitHub's limits) still give the whole command, in a code span and on a terminal.
func TestCardMergeCommandIsWhole(t *testing.T) {
	repo := strings.Repeat("o", 39) + "/" + strings.Repeat("r", 100)
	sum := summary.Summary{Room: "r", URL: rooms + "/r/r"}
	sum.Status.Phase = "AwaitingHuman"
	sum.NeedsYou = []summary.Need{{Kind: "merge", ID: "2239", What: summary.MergeHow(repo, 2239), URL: "https://github.com/" + repo + "/pull/2239"}}
	want := "gh pr merge 2239 --squash --admin --repo " + repo
	if md := card1(t, sum, Options{Format: Markdown, Now: now, Rooms: rooms}); !slices.Contains(codeSpans(md), want) {
		t.Errorf("markdown: the whole command is a code span, got %q", codeSpans(md))
	}
	if txt := card1(t, sum, textOpts(now)); !strings.Contains(txt, want) {
		t.Errorf("terminal: the whole command, got\n%s", txt)
	}
}

// The answer need names the issue's author: only a GitHub login is shown, in a code span.
func TestCardAnswerNeedIsInertForAHostileAuthor(t *testing.T) {
	for author, want := range map[string]string{"alice-b": "`@alice-b`", "_www.evil.example": "the issue's author", "x`y": "the issue's author", "": "the issue's author"} {
		sum := summary.Summary{Room: "r", URL: rooms + "/r/r"}
		sum.NeedsYou = []summary.Need{{Kind: "answer", ID: "42", Author: author, URL: "https://github.com/o/r/issues/42",
			What: "<b>evil</b> _www.evil.example answers"}}
		md := card1(t, sum, Options{Format: Markdown, Now: now, Rooms: rooms})
		assertInert(t, md, rooms, []string{"evil"})
		noEmphasis(t, md)
		if !strings.Contains(md, "1. "+want+" answers the scoper's question by editing issue #42, then a maintainer re-applies `factory/ready`") {
			t.Errorf("author %q:\n%s", author, md)
		}
	}
}

func TestCardTokenNeutralisesEmphasis(t *testing.T) {
	c := &card{o: Options{Format: Markdown, Rooms: rooms}}
	for _, w := range []string{"_x_", "a_b_", "*x*", "~x~"} {
		if got := c.token(w, false); got != "`"+w+"`" {
			t.Errorf("token(%q) = %q", w, got)
		}
	}
	if got := c.token("glm-5.3", false); got != "glm-5.3" {
		t.Errorf("token = %q", got)
	}
}

// O12: the developer's pending queued notes show on the card, quoted like the agents' notes.
func TestCardListsQueuedNotes(t *testing.T) {
	s, n := foldGolden(t, "normal")
	hostile := "# heading **bold** [x](https://evil.example) <img onerror=1>"
	s.Queued.Untrusted = true
	s.Queued.Items = []summary.QueuedNote{{Ref: 9, Author: "human:smana", Text: "use the v2 flag"}, {Ref: 10, Author: "human:smana", Text: hostile}}
	md := card1(t, s, Options{Format: Markdown, Now: n, Rooms: rooms})
	txt := card1(t, s, Options{Format: Text, Now: n, Rooms: rooms})
	for _, w := range []string{"Queued for the next run", "use the v2 flag"} {
		if !strings.Contains(md, w) || !strings.Contains(txt, w) {
			t.Errorf("no %q:\n%s\n%s", w, md, txt)
		}
	}
	assertInert(t, md, rooms, []string{"evil.example", "# heading", "**bold**", "onerror"})
	s.Queued.Items = nil
	if out := card1(t, s, Options{Format: Text, Now: n, Rooms: rooms}); strings.Contains(out, "Queued for") {
		t.Errorf("an empty queue shows a block:\n%s", out)
	}
}

// O15: the card shows the last completion judge verdict, its reason quoted.
func TestCardShowsTheLastJudgeVerdict(t *testing.T) {
	s, n := foldGolden(t, "normal")
	s.Status.LastJudge = &summary.JudgeVerdict{Verdict: "not_yet", Reason: "validate fails # x", Evidence: []string{"check:validate"}, At: n}
	md := card1(t, s, Options{Format: Markdown, Now: n, Rooms: rooms})
	txt := card1(t, s, Options{Format: Text, Now: n, Rooms: rooms})
	for _, w := range []string{"Judge", "not yet", "validate fails", "reported by the run"} {
		if !strings.Contains(md, w) || !strings.Contains(txt, w) {
			t.Errorf("no %q:\n%s\n%s", w, md, txt)
		}
	}
	assertInert(t, md, rooms, []string{"# x"})
}

// The Run row names the model and effort the run told its room, the present run's or the last's,
// only when they are that run's, and as tokens: a run's claim is never markup.
func TestCardRunRowNamesTheModelAndEffort(t *testing.T) {
	for name, c := range map[string]struct {
		current, last *summary.RunModel
		want          string
	}{
		"the present run's": {&summary.RunModel{Run: "cf4ato2x", Model: "agent-default", Effort: "high"}, nil,
			"Run   implementer cf4ato2x (human review) · agent-default · high · started 19:11 UTC, 18 min ago\n"},
		"the last run's, when it is the facts' run": {nil, &summary.RunModel{Run: "cf4ato2x", Model: "agent-default", Effort: "low"},
			"Run   implementer cf4ato2x (human review) · agent-default · low · started"},
		"an effort alone": {&summary.RunModel{Run: "cf4ato2x", Effort: "max"}, nil,
			"Run   implementer cf4ato2x (human review) · max · started"},
		"another run's is not this run's": {&summary.RunModel{Run: "zzzzzzzz", Model: "agent-default", Effort: "high"}, nil,
			"Run   implementer cf4ato2x (human review) · started"},
		"an older harness told nothing": {nil, nil, "Run   implementer cf4ato2x (human review) · started"},
	} {
		t.Run(name, func(t *testing.T) {
			s, n := foldGolden(t, "normal")
			s.Status.RunModel, s.Status.LastRunModel = c.current, c.last
			if got := card1(t, s, textOpts(n)); !strings.Contains(got, c.want) {
				t.Fatalf("want %q in\n%s", c.want, got)
			}
		})
	}
}
