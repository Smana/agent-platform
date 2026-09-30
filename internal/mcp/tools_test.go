// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/policy"
	"github.com/Smana/agent-platform/internal/runwatch"
)

type memLog struct {
	drafts []envelope.Draft
	ranged []string // the room of every Range
}

func (m *memLog) Append(_ context.Context, d envelope.Draft) (envelope.Event, bool, error) {
	m.drafts = append(m.drafts, d)
	return envelope.Event{Seq: int64(len(m.drafts)), RoomID: d.RoomID, Type: d.Type, Actor: d.Actor, Payload: d.Payload}, false, nil
}

func (m *memLog) Range(_ context.Context, room string, after int64, limit int) ([]envelope.Event, error) {
	m.ranged = append(m.ranged, room)
	var out []envelope.Event
	for i, d := range m.drafts {
		if int64(i+1) > after && len(out) < limit {
			out = append(out, envelope.Event{Seq: int64(i + 1), Type: d.Type, Actor: d.Actor, Payload: d.Payload})
		}
	}
	return out, nil
}

// fakeRedactor replaces SECRET the way redact does, and fails on FAIL.
type fakeRedactor struct{}

func (fakeRedactor) Payload(_ context.Context, raw json.RawMessage) (json.RawMessage, []string, error) {
	s := string(raw)
	if strings.Contains(s, "FAIL") {
		return nil, nil, errors.New("redact: timed out")
	}
	if !strings.Contains(s, "SECRET") {
		return raw, nil, nil
	}
	return json.RawMessage(strings.ReplaceAll(s, "SECRET", "[REDACTED:github-pat]")), []string{"github-pat"}, nil
}

func tool(tools []Tool, name string) Tool {
	for _, t := range tools {
		if t.Name == name {
			return t
		}
	}
	panic(name)
}

func tools(log Log) []Tool { return RoomTools(log, fakeRedactor{}, time.Now) }

var (
	reviewer = Caller{Run: runwatch.Run{ID: "7f3cq2xz", Room: "3kq7x2ma", Role: "reviewer", Branch: "agent/3kq7x2ma"}}
	impl     = Caller{Run: runwatch.Run{ID: "7f3cq2xz", Room: "3kq7x2ma", Role: "implementer", Branch: "agent/3kq7x2ma"}}
)

func TestVerdictIsSP3sReservedMessage(t *testing.T) {
	log := &memLog{}
	out, err := tool(tools(log), "room_verdict").Call(t.Context(), reviewer,
		json.RawMessage(`{"verdict":"changes","summary":"Missing test for the new flag.","commit":"4be1c9d"}`))
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(out); string(b) != `{"seq":1}` {
		t.Fatalf("result = %s", b)
	}
	d := log.drafts[0]
	if d.Type != envelope.Message || d.RoomID != "3kq7x2ma" || d.RunID != "7f3cq2xz" ||
		d.Actor != (envelope.Actor{Kind: envelope.ActorAgent, ID: "agent:7f3cq2xz", Role: "reviewer"}) ||
		d.Origin != envelope.OriginClient || d.OriginClient != "agent:7f3cq2xz:tools" {
		t.Fatalf("draft = %+v", d)
	}
	if err := d.Validate(); err != nil {
		t.Fatalf("the store would refuse the draft: %v", err)
	}
	var p envelope.MessagePayload
	_ = json.Unmarshal(d.Payload, &p)
	if p.Kind != envelope.KindReviewVerdict || p.Verdict != "changes" || p.Commit != "4be1c9d" ||
		p.Text != "Missing test for the new flag." || p.Delivery != envelope.DeliveryNone {
		t.Fatalf("payload = %+v", p)
	}
}

func TestPostIsAChatDeliveredToNobody(t *testing.T) {
	log := &memLog{}
	if _, err := tool(tools(log), "room_post").Call(t.Context(), impl, json.RawMessage(`{"text":"Pushed 4be1c9d.\nTests pass."}`)); err != nil {
		t.Fatal(err)
	}
	if got := string(log.drafts[0].Payload); got != `{"kind":"chat","text":"Pushed 4be1c9d.\nTests pass.","delivery":"none"}` {
		t.Fatalf("payload = %s", got)
	}
}

func TestHandoffCarriesTheBranchAndValidates(t *testing.T) {
	log := &memLog{}
	h := tool(tools(log), "room_handoff")
	if _, err := h.Call(t.Context(), impl, json.RawMessage(`{"toRole":"reviewer","summary":"Done","commit":"4be1c9d0"}`)); err != nil {
		t.Fatal(err)
	}
	d := log.drafts[0]
	if d.Type != envelope.Handoff || d.Actor.Role != "implementer" {
		t.Fatalf("draft = %+v", d)
	}
	if got := string(d.Payload); got != `{"fromRole":"implementer","toRole":"reviewer","summary":"Done","commit":"4be1c9d0","branch":"agent/3kq7x2ma"}` {
		t.Fatalf("payload = %s", got)
	}
	for _, bad := range []string{
		`{"toRole":"boss","summary":"x","commit":"4be1c9d"}`,
		`{"toRole":"reviewer","summary":"x","commit":"main"}`,
		`{"toRole":"reviewer","summary":"","commit":"4be1c9d"}`,
		`{"toRole":"reviewer","summary":"x","commit":"4BE1C9D"}`,
		`{"toRole":"reviewer","summary":"x","commit":"4be1c9"}`,
		`{"toRole":"reviewer","summary":"x","commit":"` + strings.Repeat("a", 41) + `"}`,
	} {
		if _, err := h.Call(t.Context(), impl, json.RawMessage(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if len(log.drafts) != 1 {
		t.Fatalf("%d appends, want only the valid one", len(log.drafts))
	}
}

// Every argument is untrusted model output: an unknown field, a control
// character, an oversize or blank value is refused before anything is stored,
// with an error the model can act on.
func TestArgumentsAreValidated(t *testing.T) {
	long := func(n int) string { return strings.Repeat("a", n) }
	for _, tc := range []struct {
		name, tool, args string
		ok               bool
	}{
		{"text at the cap", "room_post", `{"text":"` + long(16384) + `"}`, true},
		{"text over the cap", "room_post", `{"text":"` + long(16385) + `"}`, false},
		{"text with newlines, tabs and CR", "room_post", `{"text":"a\n\tb\r\nc"}`, true},
		{"text with a NUL", "room_post", `{"text":"a\u0000b"}`, false},
		{"text with an escape", "room_post", `{"text":"a\u001b[31mb"}`, false},
		{"text with DEL", "room_post", `{"text":"a\u007fb"}`, false},
		{"text with a C1 control", "room_post", `{"text":"a\u009b31mb"}`, false},
		{"text in another script", "room_post", `{"text":"Tests passent ✓ — 日本語"}`, true},
		{"blank text", "room_post", `{"text":"  \n "}`, false},
		{"no text", "room_post", `{}`, false},
		{"an unknown field", "room_post", `{"text":"hi","delivery":"steering"}`, false},
		{"a room of its own choosing", "room_post", `{"text":"hi","roomId":"aaaaaaaa"}`, false},
		{"not an object", "room_post", `"hi"`, false},
		{"trailing data", "room_post", `{"text":"hi"} {"text":"again"}`, false},
		{"a number for text", "room_post", `{"text":1}`, false},
		{"summary at the cap", "room_verdict", `{"verdict":"approve","summary":"` + long(8192) + `","commit":"4be1c9d"}`, true},
		{"summary over the cap", "room_verdict", `{"verdict":"approve","summary":"` + long(8193) + `","commit":"4be1c9d"}`, false},
		{"summary with a control character", "room_verdict", `{"verdict":"approve","summary":"a\u0007","commit":"4be1c9d"}`, false},
		{"an unknown verdict", "room_verdict", `{"verdict":"merge","summary":"x","commit":"4be1c9d"}`, false},
		{"a verdict in capitals", "room_verdict", `{"verdict":"APPROVE","summary":"x","commit":"4be1c9d"}`, false},
		{"a handoff summary with a control character", "room_handoff", `{"toRole":"reviewer","summary":"a\u0000","commit":"4be1c9d"}`, false},
		{"a negative sinceSeq", "room_read", `{"sinceSeq":-1}`, false},
		{"a fractional limit", "room_read", `{"limit":1.5}`, false},
		{"no read arguments", "room_read", ``, true},
		{"null read arguments", "room_read", `null`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := &memLog{}
			c := reviewer
			if tc.tool == "room_handoff" {
				c = impl
			}
			_, err := tool(tools(log), tc.tool).Call(t.Context(), c, json.RawMessage(tc.args))
			var bad argError
			switch {
			case tc.ok && err != nil:
				t.Fatalf("refused: %v", err)
			case !tc.ok && !errors.As(err, &bad):
				t.Fatalf("err = %v, want an argument error", err)
			case !tc.ok && len(log.drafts) != 0:
				t.Fatal("stored a refused call")
			}
		})
	}
}

func TestEveryWriteIsRedactedBeforeTheAppend(t *testing.T) {
	for name, args := range map[string]string{
		"room_post":    `{"text":"token SECRET"}`,
		"room_handoff": `{"toRole":"reviewer","summary":"token SECRET","commit":"4be1c9d"}`,
		"room_verdict": `{"verdict":"approve","summary":"token SECRET","commit":"4be1c9d"}`,
	} {
		t.Run(name, func(t *testing.T) {
			log := &memLog{}
			c := reviewer
			if name == "room_handoff" {
				c = impl
			}
			if _, err := tool(tools(log), name).Call(t.Context(), c, json.RawMessage(args)); err != nil {
				t.Fatal(err)
			}
			d := log.drafts[0]
			if strings.Contains(string(d.Payload), "SECRET") || !strings.Contains(string(d.Payload), "[REDACTED:github-pat]") {
				t.Fatalf("payload = %s", d.Payload)
			}
			if strings.Join(d.Redactions, ",") != "github-pat" {
				t.Fatalf("redactions = %v", d.Redactions)
			}
		})
	}
	t.Run("a failed redaction stores nothing", func(t *testing.T) {
		log := &memLog{}
		if _, err := tool(tools(log), "room_post").Call(t.Context(), reviewer, json.RawMessage(`{"text":"FAIL"}`)); err == nil || len(log.drafts) != 0 {
			t.Fatalf("err = %v, %d appends", err, len(log.drafts))
		}
	})
	t.Run("no redactor stores nothing", func(t *testing.T) {
		log := &memLog{}
		post := tool(RoomTools(log, nil, time.Now), "room_post")
		if _, err := post.Call(t.Context(), reviewer, json.RawMessage(`{"text":"hi"}`)); err == nil || len(log.drafts) != 0 {
			t.Fatalf("err = %v, %d appends", err, len(log.drafts))
		}
	})
}

// P26: MCP carries no retry key, so each call is its own entry, keyed by the
// injected clock.
func TestEachCallIsItsOwnEntry(t *testing.T) {
	log := &memLog{}
	c := newClock()
	post := tool(RoomTools(log, fakeRedactor{}, c.now), "room_post")
	for range 2 {
		if _, err := post.Call(t.Context(), reviewer, json.RawMessage(`{"text":"hi"}`)); err != nil {
			t.Fatal(err)
		}
		c.add(time.Nanosecond)
	}
	if a, b := log.drafts[0].OriginSeq, log.drafts[1].OriginSeq; a != c.t.Add(-2*time.Nanosecond).UnixNano() || b != a+1 {
		t.Fatalf("originSeq = %d, %d", a, b)
	}
}

func TestReadReturnsMessagesAndHandoffsOnly(t *testing.T) {
	log := &memLog{}
	log.drafts = []envelope.Draft{
		{Type: envelope.ToolCall, Payload: []byte(`{}`)},
		{Type: envelope.Message, Payload: []byte(`{"kind":"chat","text":"hi","delivery":"none"}`)},
		{Type: envelope.Handoff, Payload: []byte(`{"toRole":"reviewer"}`)},
		{Type: envelope.StateChanged, Payload: []byte(`{"kind":"run_phase"}`)},
	}
	out, err := tool(tools(log), "room_read").Call(t.Context(), reviewer, json.RawMessage(`{"sinceSeq":0}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	if strings.Contains(string(b), "tool_call") || strings.Contains(string(b), "state_changed") ||
		!strings.Contains(string(b), `"type":"handoff"`) || !strings.Contains(string(b), `"type":"message"`) ||
		!strings.Contains(string(b), `"lastSeq":4`) {
		t.Fatalf("%s", b)
	}
	if strings.Join(log.ranged, ",") != "3kq7x2ma" {
		t.Fatalf("read rooms %v, want only the run's own", log.ranged)
	}
}

type readResult struct {
	Events []envelope.Event `json:"events"`
	Last   int64            `json:"lastSeq"`
}

func read(t *testing.T, log Log, args string) readResult {
	t.Helper()
	out, err := tool(tools(log), "room_read").Call(t.Context(), reviewer, json.RawMessage(args))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(out)
	var r readResult
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

// A reader that pages with lastSeq sees every message once: a page cut at the
// limit resumes at the next unread message, and a read with nothing new keeps
// the reader where it was.
func TestReadPagesWithoutLosingEvents(t *testing.T) {
	log := &memLog{}
	for i := range 7 {
		typ := envelope.Message
		if i%2 == 1 {
			typ = envelope.ToolResult
		}
		log.drafts = append(log.drafts, envelope.Draft{Type: typ, Payload: []byte(fmt.Sprintf(`{"n":%d}`, i+1))})
	}
	var seen []int64
	since := int64(0)
	for range 5 {
		r := read(t, log, fmt.Sprintf(`{"sinceSeq":%d,"limit":2}`, since))
		if len(r.Events) > 2 {
			t.Fatalf("%d events, limit 2", len(r.Events))
		}
		for _, e := range r.Events {
			seen = append(seen, e.Seq)
		}
		if r.Last < since {
			t.Fatalf("lastSeq went back from %d to %d", since, r.Last)
		}
		since = r.Last
	}
	if fmt.Sprint(seen) != "[1 3 5 7]" {
		t.Fatalf("seen %v, want every message once", seen)
	}
	if r := read(t, log, `{"sinceSeq":7}`); r.Last != 7 || len(r.Events) != 0 {
		t.Fatalf("nothing new: %+v", r)
	}
	for range 150 {
		log.drafts = append(log.drafts, envelope.Draft{Type: envelope.Message, Payload: []byte(`{}`)})
	}
	if r := read(t, log, `{"sinceSeq":0,"limit":1000}`); len(r.Events) != maxRead {
		t.Fatalf("a limit over 100 is clamped, not refused: %d events", len(r.Events))
	}
	if r := read(t, log, `{}`); len(r.Events) != maxRead {
		t.Fatalf("no limit is 100: %d events", len(r.Events))
	}
}

// room_read scans at most readScan events a call, so a room full of tool calls
// still answers quickly, and lastSeq moves past what was scanned.
func TestReadScansABoundedWindow(t *testing.T) {
	log := &memLog{}
	for range readScan + 3 {
		log.drafts = append(log.drafts, envelope.Draft{Type: envelope.ToolCall, Payload: []byte(`{}`)})
	}
	log.drafts = append(log.drafts, envelope.Draft{Type: envelope.Message, Payload: []byte(`{}`)})
	r := read(t, log, `{}`)
	if len(r.Events) != 0 || r.Last != readScan {
		t.Fatalf("%d events, lastSeq %d, want 0 and %d", len(r.Events), r.Last, readScan)
	}
	if r := read(t, log, fmt.Sprintf(`{"sinceSeq":%d}`, r.Last)); len(r.Events) != 1 {
		t.Fatalf("the next page holds the message: %+v", r)
	}
}

func TestRolesMatchTheSpecTable(t *testing.T) {
	want := map[string]struct {
		roles  []string
		action policy.Action
	}{
		"room_read":    {[]string{"implementer", "reviewer", "tester", "triager"}, policy.Read},
		"room_post":    {[]string{"implementer", "reviewer", "tester", "triager"}, policy.Chat},
		"room_handoff": {[]string{"implementer", "tester", "triager"}, policy.Chat},
		"room_verdict": {[]string{"reviewer", "tester"}, policy.Chat},
	}
	got := tools(&memLog{})
	if len(got) != len(want) {
		t.Fatalf("%d tools", len(got))
	}
	for _, t2 := range got {
		w := want[t2.Name]
		if strings.Join(t2.Roles, ",") != strings.Join(w.roles, ",") || t2.Action != w.action {
			t.Errorf("%s roles = %v, action %q", t2.Name, t2.Roles, t2.Action)
		}
		if !json.Valid(t2.InputSchema) || t2.Description == "" {
			t.Errorf("%s: schema or description missing", t2.Name)
		}
	}
}

// The tools behind the server: a reviewer's verdict lands in its own room, and
// an implementer cannot reach room_verdict at all.
func TestToolsBehindTheServer(t *testing.T) {
	log := &memLog{}
	s, _ := testServer(watcher(t, "reviewer"), newClock())
	s.Tools = tools(log)
	res, text := callWith(t, s, "room_verdict", map[string]any{"verdict": "approve", "summary": "LGTM", "commit": "4be1c9d"})
	if res["isError"] == true || text != `{"seq":1}` || log.drafts[0].RoomID != "3kq7x2ma" {
		t.Fatalf("%v %q", res, text)
	}
	s, _ = testServer(watcher(t, "implementer"), newClock())
	s.Tools = tools(log)
	if res, _ := callWith(t, s, "room_verdict", map[string]any{"verdict": "approve", "summary": "LGTM", "commit": "4be1c9d"}); res["isError"] != true || len(log.drafts) != 1 {
		t.Fatalf("an implementer's verdict: %v", res)
	}
}

func callWith(t *testing.T, s *Server, name string, args any) (map[string]any, string) {
	t.Helper()
	res, _ := rpc(t, s, "k", sub, "tools/call", map[string]any{"name": name, "arguments": args}).out["result"].(map[string]any)
	text := ""
	if c, ok := res["content"].([]any); ok && len(c) == 1 {
		text, _ = c[0].(map[string]any)["text"].(string)
	}
	return res, text
}

func TestVerdictNamesThePullRequestUnderReview(t *testing.T) {
	const prURL = "https://github.com/Smana/cloud-native-ref/pull/12"
	args := json.RawMessage(`{"verdict":"approve","summary":"Looks right.","commit":"4be1c9d"}`)
	for _, tc := range []struct {
		name, repo, task, want string
	}{
		{"a pull request of the run's repository", "Smana/cloud-native-ref", prURL, prURL},
		{"another repository's pull request", "Smana/cloud-native-ref", "https://github.com/someone/else/pull/3", ""},
		{"a repository whose name extends the run's", "Smana/cloud-native-ref", "https://github.com/Smana/cloud-native-ref-evil/pull/3", ""},
		{"an issue, not a pull request", "Smana/cloud-native-ref", "https://github.com/Smana/cloud-native-ref/issues/12", ""},
		{"a pull request page, not the pull request", "Smana/cloud-native-ref", prURL + "/files", ""},
		{"pull request zero", "Smana/cloud-native-ref", "https://github.com/Smana/cloud-native-ref/pull/0", ""},
		{"plain http", "Smana/cloud-native-ref", "http://github.com/Smana/cloud-native-ref/pull/12", ""},
		{"a run with no repository", "", prURL, ""},
		{"a run with no task", "Smana/cloud-native-ref", "", ""},
		{"a repository with regexp in its name", "Smana/cloud.native-ref", "https://github.com/Smana/cloudXnative-ref/pull/12", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log := &memLog{}
			rev := Caller{Run: runwatch.Run{ID: "7f3cq2xz", Room: "3kq7x2ma", Role: "reviewer", Repository: tc.repo, TaskURL: tc.task}}
			if _, err := tool(tools(log), "room_verdict").Call(t.Context(), rev, args); err != nil {
				t.Fatal(err)
			}
			var p envelope.MessagePayload
			_ = json.Unmarshal(log.drafts[0].Payload, &p)
			if p.PullRequest != tc.want {
				t.Fatalf("pullRequest = %q, want %q", p.PullRequest, tc.want)
			}
		})
	}
}

// A chat never names a pull request: only a verdict is posted on one.
func TestAChatNamesNoPullRequest(t *testing.T) {
	log := &memLog{}
	rev := Caller{Run: runwatch.Run{ID: "7f3cq2xz", Room: "3kq7x2ma", Role: "reviewer", Repository: "Smana/cloud-native-ref",
		TaskURL: "https://github.com/Smana/cloud-native-ref/pull/12"}}
	if _, err := tool(tools(log), "room_post").Call(t.Context(), rev, json.RawMessage(`{"text":"hi"}`)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(log.drafts[0].Payload), "pullRequest") {
		t.Fatalf("%s", log.drafts[0].Payload)
	}
}
