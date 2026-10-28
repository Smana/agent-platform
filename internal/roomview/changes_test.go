// SPDX-License-Identifier: Apache-2.0

package roomview

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Smana/agent-platform/internal/envelope"
)

const (
	changesRoom = "26zfnuxm"
	pr2239      = "https://github.com/Smana/cloud-native-ref/pull/2239"
)

var changesBase = time.Date(2026, 10, 7, 19, 0, 0, 0, time.UTC)

func at(mins, sec int) time.Time {
	return changesBase.Add(time.Duration(mins)*time.Minute + time.Duration(sec)*time.Second)
}

func ev(seq int64, ts time.Time, run string, typ envelope.Type, payload any) envelope.Event {
	return envelope.Event{V: 1, Seq: seq, RoomID: changesRoom, RunID: run, Type: typ, TS: ts,
		Actor: envelope.Actor{Kind: envelope.ActorAgent, ID: "agent:" + run, Role: "implementer"}, Payload: envelope.Must(payload)}
}

func factEv(seq int64, ts time.Time, f envelope.TaskFacts) envelope.Event {
	return envelope.Event{V: 1, Seq: seq, RoomID: changesRoom, Type: envelope.StateChanged, TS: ts,
		Actor: envelope.Actor{Kind: envelope.ActorSystem, ID: "factory"}, Payload: envelope.TaskStatePayload(f)}
}

func note(seq int64, ts time.Time, text string) envelope.Event {
	return ev(seq, ts, "cf4ato2x", envelope.Message, envelope.MessagePayload{Kind: envelope.KindProgress, Text: text, Delivery: envelope.DeliveryNone})
}

func milestones(base *envelope.TaskFacts, evs ...envelope.Event) ChangeSet {
	return Milestones(MilestoneInput{Room: changesRoom, URL: rooms + "/r/" + changesRoom, Base: base, Events: evs})
}

func kindsAndText(c ChangeSet) []string {
	var out []string
	for _, m := range c.Items {
		out = append(out, m.Kind+": "+m.Text)
	}
	return out
}

func run(id, role, trigger string) envelope.RunFact {
	return envelope.RunFact{ID: id, Role: role, Trigger: trigger, StartedAt: at(1, 0)}
}

func TestMilestonesFromFacts(t *testing.T) {
	pr := &envelope.PRFact{Number: 2239, URL: pr2239}
	lost := run("a", "implementer", "initial")
	lost.EndedAt, lost.Outcome = at(2, 0), envelope.OutcomeLost
	cases := []struct {
		name string
		base *envelope.TaskFacts
		page envelope.TaskFacts
		want []string
	}{
		{"pr opened", &envelope.TaskFacts{Phase: "Implementing"}, envelope.TaskFacts{Phase: "Implementing", PR: pr},
			[]string{"pr_opened: pull request #2239 opened"}},
		{"lost then resume",
			&envelope.TaskFacts{Phase: "Implementing", Runs: []envelope.RunFact{run("a", "implementer", "initial")}},
			envelope.TaskFacts{Phase: "Implementing", Runs: []envelope.RunFact{lost, run("b", "implementer", "resume")}},
			[]string{"run_ended: the sandbox was lost", "run_started: the factory started implementer `b` (resume 1)"}},
		{"human review",
			&envelope.TaskFacts{Phase: "AwaitingHuman", PR: pr, Runs: []envelope.RunFact{run("a", "implementer", "initial")}},
			envelope.TaskFacts{Phase: "Implementing", PR: pr, Runs: []envelope.RunFact{run("a", "implementer", "initial"), run("b", "implementer", "human")}},
			[]string{"run_started: changes were requested on #2239 → the factory started implementer `b` (human review)"}},
		{"merged", &envelope.TaskFacts{Phase: "AutoMerging", PR: pr}, envelope.TaskFacts{Phase: "Merged", PR: pr},
			[]string{"merged: #2239 merged, verifying"}},
		{"reverted", &envelope.TaskFacts{Phase: "Merged", PR: pr}, envelope.TaskFacts{Phase: "Reverted", PR: pr},
			[]string{"reverted: #2239 merge reverted"}},
		{"held", &envelope.TaskFacts{Phase: "Queued"}, envelope.TaskFacts{Phase: "Queued", Reason: "waiting_human_driver"},
			[]string{"held: held while a person drives the room"}},
		{"resumed", &envelope.TaskFacts{Phase: "Queued", Reason: "waiting_human_driver"}, envelope.TaskFacts{Phase: "Implementing"},
			[]string{"resumed: the hold ended: an agent is coding"}},
		{"budget reached by dollars",
			&envelope.TaskFacts{Phase: "Implementing", Spend: &envelope.SpendFact{UsedMicroUSD: 900000, LimitMicroUSD: 1500000}},
			envelope.TaskFacts{Phase: "Implementing", Spend: &envelope.SpendFact{UsedMicroUSD: 1500000, LimitMicroUSD: 1500000}},
			[]string{"budget_reached: the budget is reached: $1.50 of $1.50"}},
		{"budget reached by tokens",
			&envelope.TaskFacts{Phase: "Implementing", Budget: &envelope.BudgetFact{UsedTokens: 900000, LimitTokens: 1000000}},
			envelope.TaskFacts{Phase: "Implementing", Budget: &envelope.BudgetFact{UsedTokens: 1000000, LimitTokens: 1000000}},
			[]string{"budget_reached: the budget is reached: 1 M of 1 M tokens"}},
		{"budget stays reached",
			&envelope.TaskFacts{Phase: "Implementing", Spend: &envelope.SpendFact{UsedMicroUSD: 1500000, LimitMicroUSD: 1500000}},
			envelope.TaskFacts{Phase: "Implementing", Spend: &envelope.SpendFact{UsedMicroUSD: 1600000, LimitMicroUSD: 1500000}},
			nil},
		// A nil base means the room had no facts before the window: its first PR and run are news.
		{"nil base, the pr is news", nil, envelope.TaskFacts{Phase: "Implementing", PR: pr},
			[]string{"pr_opened: pull request #2239 opened"}},
		{"nil base, the first run is news", nil, envelope.TaskFacts{Phase: "Implementing", Runs: []envelope.RunFact{run("a", "implementer", "initial")}},
			[]string{"run_started: the factory started implementer `a` (first run)"}},
		{"budget reached by tokens of a priced task reads dollars",
			&envelope.TaskFacts{Phase: "Implementing", Budget: &envelope.BudgetFact{UsedTokens: 900000, LimitTokens: 1000000}, Spend: &envelope.SpendFact{UsedMicroUSD: 310000}},
			envelope.TaskFacts{Phase: "Implementing", Budget: &envelope.BudgetFact{UsedTokens: 1000000, LimitTokens: 1000000}, Spend: &envelope.SpendFact{UsedMicroUSD: 310000}},
			[]string{"budget_reached: the budget is reached: $0.31 spent"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := kindsAndText(milestones(c.base, factEv(5, at(3, 0), c.page)))
			if strings.Join(got, "\n") != strings.Join(c.want, "\n") {
				t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(c.want, "\n"))
			}
		})
	}
}

func TestMilestonesURLs(t *testing.T) {
	pr := &envelope.PRFact{Number: 2239, URL: pr2239}
	c := milestones(&envelope.TaskFacts{Phase: "Implementing"}, factEv(5, at(3, 0), envelope.TaskFacts{Phase: "Implementing", PR: pr}))
	if len(c.Items) != 1 || c.Items[0].URL != pr2239 || c.Items[0].Seq != 5 {
		t.Errorf("pr_opened = %+v", c.Items)
	}
	if !c.Untrusted || c.Room != changesRoom {
		t.Errorf("envelope facts: %+v", c)
	}
}

func TestMilestonesFromEvents(t *testing.T) {
	req := envelope.ApprovalRequestedPayload{ApprovalID: "approval-a1", CallID: "c1", Class: "forge.push",
		Action: json.RawMessage(`{"command":"git push to agent/26zfnuxm"}`), ExpiresAt: at(11, 0)}
	c := milestones(nil,
		ev(1, at(0, 0), "cf4ato2x", envelope.ApprovalRequested, req),
		ev(2, at(1, 0), "", envelope.ApprovalDecided, envelope.ApprovalDecidedPayload{ApprovalID: "approval-a1", Decision: "approved"}),
		ev(3, at(2, 0), "cf4ato2x", envelope.Message, envelope.MessagePayload{Kind: envelope.KindReviewVerdict, Verdict: "approve", PullRequest: pr2239}),
		ev(4, at(3, 0), "cf4ato2x", envelope.Handoff, envelope.HandoffPayload{FromRole: "implementer", ToRole: "reviewer", Summary: "done"}),
		ev(5, at(4, 0), "", envelope.StateChanged, map[string]any{"kind": "room_phase", "phase": "Closed"}),
		ev(6, at(5, 0), "", envelope.StateChanged, map[string]any{"kind": "limit", "reason": "run", "running": true}),
		ev(7, at(6, 0), "", envelope.StateChanged, map[string]any{"kind": "limit", "events": 100, "bytes": 5}),
		ev(8, at(7, 0), "cf4ato2x", envelope.Message, envelope.MessagePayload{Kind: envelope.KindChat, Text: "chat is not a milestone"}),
	)
	var kinds []string
	for _, m := range c.Items {
		kinds = append(kinds, m.Kind)
	}
	want := "approval_requested approval_decided verdict handoff ended ended"
	if strings.Join(kinds, " ") != want {
		t.Fatalf("kinds = %v, want %s", kinds, want)
	}
	if m := c.Items[0]; m.URL != rooms+"/r/"+changesRoom+"#approval-a1" || m.Text != "git push to agent/26zfnuxm" || !m.Until.Equal(at(11, 0)) {
		t.Errorf("approval_requested = %+v", m)
	}
	if m := c.Items[2]; m.URL != pr2239 || m.Text != "approved" {
		t.Errorf("verdict = %+v", m)
	}
	if !c.Since.Equal(at(0, 0)) || c.Cursor != "seq:8" {
		t.Errorf("since %v cursor %q", c.Since, c.Cursor)
	}
}

func TestMilestonesCollapseActivity(t *testing.T) {
	var evs []envelope.Event
	seq := int64(1)
	add := func(ts time.Time, typ envelope.Type, p any) {
		evs = append(evs, ev(seq, ts, "cf4ato2x", typ, p))
		seq++
	}
	for i := 0; i < 9; i++ {
		ts := at(11+i*5/9, i)
		add(ts, envelope.Turn, envelope.TurnPayload{RunID: "cf4ato2x", TurnID: fmt.Sprint("t", i), Phase: "started"})
		add(ts, envelope.Turn, envelope.TurnPayload{RunID: "cf4ato2x", TurnID: fmt.Sprint("t", i), Phase: "completed"})
	}
	for i := 0; i < 14; i++ {
		class := "shell"
		if i == 6 {
			class = "forge.push"
		}
		add(at(11, 30+i), envelope.ToolCall, envelope.ToolCallPayload{CallID: fmt.Sprint("c", i), Tool: "bash", Class: class})
	}
	last := at(16, 0)
	add(last, envelope.Turn, envelope.TurnPayload{RunID: "cf4ato2x", TurnID: "t9", Phase: "started"})
	c := milestones(nil, evs...)
	if len(c.Items) != 1 {
		t.Fatalf("items = %+v", c.Items)
	}
	m := c.Items[0]
	if m.Kind != "activity" || m.Run != "cf4ato2x" || m.Turns != 9 || m.Tools != 14 || m.Pushes != 1 {
		t.Errorf("activity = %+v", m)
	}
	if !m.At.Equal(evs[0].TS) || !m.Until.Equal(last) || m.Seq != seq-1 {
		t.Errorf("span %v–%v seq %d", m.At, m.Until, m.Seq)
	}
}

func TestMilestonesPageAt30(t *testing.T) {
	var evs []envelope.Event
	for i := int64(1); i <= 40; i++ {
		evs = append(evs, note(i*2, at(int(i), 0), fmt.Sprint("note ", i)))
	}
	c := milestones(nil, evs...)
	if len(c.Items) != 30 || c.More != 10 || c.Cursor != "seq:60" {
		t.Errorf("items %d more %d cursor %q", len(c.Items), c.More, c.Cursor)
	}
	if got := Milestones(MilestoneInput{Room: changesRoom, Events: evs, Limit: 5}); len(got.Items) != 5 || got.More != 35 || got.Cursor != "seq:10" {
		t.Errorf("limit 5: %d %d %q", len(got.Items), got.More, got.Cursor)
	}
}

func TestMilestonesJSON(t *testing.T) {
	b, err := json.Marshal(milestones(nil, note(1, at(0, 0), "x")))
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{`"room"`, `"url"`, `"since"`, `"items"`, `"more"`, `"cursor"`, `"untrusted":true`, `"seq"`, `"kind":"note"`, `"role":"implementer"`} {
		if !strings.Contains(string(b), k) {
			t.Errorf("%s lacks %s", b, k)
		}
	}
	if strings.Contains(string(b), `"until"`) {
		t.Errorf("a zero until is omitted: %s", b)
	}
}

// specChangesLog is view 4's example: a person asked for changes, the run was lost and resumed, and
// an approval waits.
func specChangesLog() MilestoneInput {
	pr := &envelope.PRFact{Number: 2239, URL: pr2239}
	first := run("first001", "implementer", "initial")
	first.EndedAt, first.Outcome = at(-30, 0), envelope.OutcomeSucceeded
	human := run("cf4ato2x", "implementer", "human")
	human.StartedAt = at(11, 0)
	lostRun := human
	lostRun.EndedAt, lostRun.Outcome = at(16, 0), envelope.OutcomeLost
	resume := run("huwttc64", "implementer", "resume")
	resume.StartedAt = at(18, 0)

	evs := []envelope.Event{
		ev(1, at(5, 0), "", envelope.Participant, envelope.ParticipantPayload{Principal: "human:own", Change: "joined"}),
		factEv(2, at(11, 0), envelope.TaskFacts{Phase: "Implementing", PR: pr, Runs: []envelope.RunFact{first, human}}),
		note(3, at(12, 0), "found both versions on lines 12–13, fixing"),
	}
	seq := int64(4)
	for i := 0; i < 9; i++ {
		evs = append(evs, ev(seq, at(11, i*30), "cf4ato2x", envelope.Turn, envelope.TurnPayload{RunID: "cf4ato2x", TurnID: fmt.Sprint(i), Phase: "completed"}))
		seq++
	}
	for i := 0; i < 14; i++ {
		class := "shell"
		if i == 13 {
			class = "forge.push"
		}
		ts := at(11, 5+i)
		if i == 13 {
			ts = at(16, 0)
		}
		evs = append(evs, ev(seq, ts, "cf4ato2x", envelope.ToolCall, envelope.ToolCallPayload{CallID: fmt.Sprint("c", i), Tool: "bash", Class: class}))
		seq++
	}
	evs = append(evs,
		factEv(seq, at(16, 5), envelope.TaskFacts{Phase: "Implementing", PR: pr, Runs: []envelope.RunFact{first, lostRun}}),
		factEv(seq+1, at(18, 5), envelope.TaskFacts{Phase: "Implementing", PR: pr, Runs: []envelope.RunFact{first, lostRun, resume}}),
		ev(seq+2, at(19, 0), "huwttc64", envelope.ApprovalRequested, envelope.ApprovalRequestedPayload{ApprovalID: "approval-a1", CallID: "c9", Class: "forge.push",
			Action: json.RawMessage(`{"command":"git push to agent/26zfnuxm"}`), ExpiresAt: at(30, 0)}),
	)
	return MilestoneInput{Room: changesRoom, URL: rooms + "/r/" + changesRoom, Events: evs,
		Base: &envelope.TaskFacts{Phase: "AwaitingHuman", PR: pr, Runs: []envelope.RunFact{first}}}
}

func changes1(t *testing.T, c ChangeSet, o Options) string {
	t.Helper()
	var b bytes.Buffer
	if err := Changes(&b, c, o); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestChangesGolden(t *testing.T) {
	n := at(19, 30)
	c := Milestones(specChangesLog())
	md := changes1(t, c, Options{Format: Markdown, Now: n, Rooms: rooms})
	golden(t, "changes_spec.md", md)
	golden(t, "changes_spec.txt", changes1(t, c, Options{Format: Text, Now: n, Rooms: rooms}))
	assertInert(t, md, rooms, nil)
	want := `**26zfnuxm · since 19:05 UTC** (14 min)

- 19:11 changes were requested on [#2239](https://github.com/Smana/cloud-native-ref/pull/2239) → the factory started implementer ` + "`cf4ato2x`" + ` (human review)
- 19:12 note ` + "`found both versions on lines 12–13, fixing`" + `
- 19:11–19:16 ` + "`cf4ato2x`" + `: 9 model turns, 14 tool calls, 1 push
- 19:16 the sandbox was lost
- 19:18 the factory started implementer ` + "`huwttc64`" + ` (resume 1)
- 19:19 **approval requested**: ` + "`git push to agent/26zfnuxm`" + `, by 19:30 UTC → [decide in the room](https://rooms.example/r/26zfnuxm#approval-a1)
`
	if md != want {
		t.Errorf("the view is not the spec's, apart from R19:\n--- want\n%s\n--- got\n%s", want, md)
	}
}

func TestChangesMore(t *testing.T) {
	var evs []envelope.Event
	for i := int64(1); i <= 40; i++ {
		evs = append(evs, note(i, at(int(i), 0), "n"))
	}
	c := milestones(nil, evs...)
	md := changes1(t, c, Options{Format: Markdown, Now: at(50, 0), Rooms: rooms})
	if strings.Count(md, "\n- ") != 30 || !strings.HasSuffix(md, "\n10 more\n") {
		t.Errorf("30 lines then 10 more:\n%s", md)
	}
	if empty := changes1(t, milestones(nil), Options{Format: Markdown, Now: at(50, 0), Rooms: rooms}); !strings.Contains(empty, "Nothing changed") {
		t.Errorf("empty view: %q", empty)
	}
}

func TestChangesHostile(t *testing.T) {
	b, err := os.ReadFile("../summary/testdata/hostile.json")
	if err != nil {
		t.Fatal(err)
	}
	var tc struct {
		Events []envelope.Event `json:"events"`
		Now    time.Time        `json:"now"`
	}
	if err := json.Unmarshal(b, &tc); err != nil {
		t.Fatal(err)
	}
	c := Milestones(MilestoneInput{Room: changesRoom, URL: rooms + "/r/" + changesRoom, Events: tc.Events})
	if len(c.Items) == 0 {
		t.Fatal("the hostile fixture has milestones")
	}
	md := changes1(t, c, Options{Format: Markdown, Now: tc.Now, Rooms: rooms})
	assertInert(t, md, rooms, []string{"evil", "heading", "bold", "gone", "onerror"})
	golden(t, "changes_hostile.md", md)
	golden(t, "changes_hostile.txt", changes1(t, c, Options{Format: Text, Now: tc.Now, Rooms: rooms}))
	for _, line := range strings.Split(md, "\n") {
		if strings.Contains(line, "\u200b") || strings.Contains(line, "\u202e") || strings.Contains(line, "\x1b") {
			t.Errorf("a hidden character reaches %q", line)
		}
	}
}

func TestChangesHostileFacts(t *testing.T) {
	h := "x`**\u202e[a](https://evil.example)"
	c := milestones(&envelope.TaskFacts{Phase: "Implementing"},
		factEv(2, at(1, 0), envelope.TaskFacts{Phase: "Implementing", Runs: []envelope.RunFact{
			{ID: h, Role: h, Trigger: h, StartedAt: at(1, 0), EndedAt: at(2, 0), Outcome: ""},
		}}))
	md := changes1(t, c, Options{Format: Markdown, Now: at(3, 0), Rooms: rooms})
	assertInert(t, md, rooms, []string{"evil", "**"})
	if strings.Contains(md, "\u202e") {
		t.Errorf("bidi in %q", md)
	}
}

// A fallback value the vocabulary lacks is untrusted: an autolinking word never renders as a link.
func TestChangesHostileFactsAutolink(t *testing.T) {
	for _, h := range []string{"https://evil.example", "www.evil.example", "a@b.example", "(www.evil.example)", "*www.evil.example*"} {
		c := milestones(&envelope.TaskFacts{Phase: "Implementing"},
			factEv(2, at(1, 0), envelope.TaskFacts{Phase: "Implementing", Runs: []envelope.RunFact{
				{ID: "r1", Role: "implementer", Trigger: h, StartedAt: at(1, 0), EndedAt: at(2, 0), Outcome: h},
			}}))
		md := changes1(t, c, Options{Format: Markdown, Now: at(3, 0), Rooms: rooms})
		assertInert(t, md, rooms, nil)
		if !strings.Contains(strings.Join(codeSpans(md), "\n"), h) {
			t.Errorf("%q is not in a code span:\n%s", h, md)
		}
	}
}

// The page ends on a seq boundary: the milestones of one event are never split across two pages.
func TestMilestonesCutAtASeqBoundary(t *testing.T) {
	pr := &envelope.PRFact{Number: 2239, URL: pr2239}
	var evs []envelope.Event
	for i := 1; i <= 29; i++ {
		evs = append(evs, note(int64(i), at(i, 0), fmt.Sprintf("n%d", i)))
	}
	// One fact event is three milestones at seq 30: item 30 of the page falls inside it.
	evs = append(evs, factEv(30, at(30, 0), envelope.TaskFacts{Phase: "Merged", PR: pr,
		Runs: []envelope.RunFact{run("b", "implementer", "retry")}}))
	c := milestones(&envelope.TaskFacts{Phase: "Implementing"}, evs...)
	if len(c.Items) != 29 || c.More != 3 || c.Cursor != "seq:29" {
		t.Errorf("items %d, more %d, cursor %q; want 29, 3, seq:29", len(c.Items), c.More, c.Cursor)
	}
	// A seq that alone outruns the limit is shown whole, and nothing is left behind.
	c = Milestones(MilestoneInput{Room: changesRoom, Base: &envelope.TaskFacts{Phase: "Implementing"}, Limit: 2, Events: evs[29:]})
	if len(c.Items) != 3 || c.More != 0 || c.Cursor != "seq:30" {
		t.Errorf("one seq of 3 with limit 2: items %d, more %d, cursor %q", len(c.Items), c.More, c.Cursor)
	}
}

func TestChangeSetSinceIsOmittedWhenEmpty(t *testing.T) {
	b, err := json.Marshal(Milestones(MilestoneInput{Room: changesRoom}))
	if err != nil || strings.Contains(string(b), "since") {
		t.Errorf("%s %v", b, err)
	}
}

// O15: a judge verdict is a milestone, with its reason quoted as the agent's text.
func TestMilestonesListTheJudgesVerdict(t *testing.T) {
	j := ev(5, at(9, 0), "cf4ato2x", envelope.Message, envelope.MessagePayload{Kind: envelope.KindJudgeVerdict, Verdict: "not_yet",
		Text: "check:validate failed **bold**", Evidence: []string{"check:validate"}, Delivery: envelope.DeliveryNone})
	c := milestones(nil, j)
	if len(c.Items) != 1 || c.Items[0].Kind != "judge" || c.Items[0].Verdict != "not yet" {
		t.Fatalf("%+v", c.Items)
	}
	var b bytes.Buffer
	if err := Changes(&b, c, Options{Format: Markdown, Now: at(10, 0), Rooms: rooms}); err != nil {
		t.Fatal(err)
	}
	if out := b.String(); !strings.Contains(out, "completion judge: not yet") || !strings.Contains(out, "`check:validate failed **bold**`") {
		t.Fatalf("%s", out)
	}
}
