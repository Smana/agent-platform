// SPDX-License-Identifier: Apache-2.0

package roomctl

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Smana/agent-platform/internal/roomview"
	"github.com/Smana/agent-platform/internal/summary"
)

func TestHit(t *testing.T) {
	run := roomview.Row{ID: "a", State: "running", LastSeq: 5}
	row := func(f func(*roomview.Row)) roomview.Row { r := run; f(&r); return r }
	needs := func(r *roomview.Row) { r.NeedsYou = 1 }
	someone := func(r *roomview.Row) { r.NeedsMe = true }
	pr := func(r *roomview.Row) { r.PR = 7 }
	ended := func(r *roomview.Row) { r.State = "done" }
	moved := func(r *roomview.Row) { r.LastSeq = 9 }
	newNeed := func(id string) func(*roomview.Row) {
		return func(r *roomview.Row) { r.Need = &summary.Need{Kind: "approval", ID: id} }
	}
	for name, tc := range map[string]struct {
		base, now roomview.Row
		until     []string
		want      string
	}{
		"needs_you newly true":           {run, row(needs), []string{"needs_you"}, "needs_you"},
		"needs_you, someone else's need": {run, row(someone), []string{"needs_you"}, ""},
		"default: someone else's need":   {run, row(someone), nil, ""},
		"needs_you, count 0 to 2":        {run, row(func(r *roomview.Row) { r.NeedsYou = 2 }), []string{"needs_you"}, "needs_you"},
		"needs_you already true":         {row(needs), row(needs), []string{"needs_you"}, ""},
		"needs_you, another need":        {row(newNeed("a1")), row(newNeed("a2")), []string{"needs_you"}, "needs_you"},
		"needs_you, the same need":       {row(newNeed("a1")), row(newNeed("a1")), []string{"needs_you"}, ""},
		"pr_opened newly":                {run, row(pr), []string{"pr_opened"}, "pr_opened"},
		"pr_opened already":              {row(pr), row(pr), []string{"pr_opened"}, ""},
		"ended newly":                    {run, row(ended), []string{"ended"}, "ended"},
		"ended already":                  {row(ended), row(ended), []string{"ended"}, ""},
		"any_change newly":               {run, row(moved), []string{"any_change"}, "any_change"},
		"any_change, nothing moved":      {run, run, []string{"any_change"}, ""},
		"a condition not asked for":      {run, row(pr), []string{"ended"}, ""},
		// Without until it is the first three (L13): lastSeq alone is no hit.
		"default: lastSeq alone":    {run, row(moved), nil, ""},
		"default: needs_you":        {run, row(needs), nil, "needs_you"},
		"default: pr_opened":        {run, row(pr), nil, "pr_opened"},
		"default: ended":            {run, row(ended), nil, "ended"},
		"empty until is default":    {run, row(moved), []string{}, ""},
		"first of several in order": {run, row(func(r *roomview.Row) { pr(r); ended(r) }), nil, "pr_opened"},
	} {
		t.Run(name, func(t *testing.T) {
			h, ok := hit(map[string]roomview.Row{"a": tc.base}, map[string]roomview.Row{"a": tc.now}, tc.until)
			if ok != (tc.want != "") || h.Condition != tc.want || (ok && h.Room != "a") {
				t.Fatalf("%v %v, want %q", h, ok, tc.want)
			}
		})
	}
	// A room with no snapshot is not compared.
	if h, ok := hit(map[string]roomview.Row{}, map[string]roomview.Row{"a": row(needs)}, nil); ok {
		t.Fatalf("%v", h)
	}
}

// steps is an After that records each wait and fires at once.
type steps struct {
	mu sync.Mutex
	ds []time.Duration
}

func (s *steps) after(d time.Duration) <-chan time.Time {
	s.mu.Lock()
	s.ds = append(s.ds, d)
	s.mu.Unlock()
	c := make(chan time.Time, 1)
	c <- time.Now()
	return c
}

func (s *steps) waits() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Duration(nil), s.ds...)
}

// gate is an After that never fires by itself: each wait is announced on asked, and a test
// releases them with release <- time.Now().
type gate struct {
	asked   chan time.Duration
	release chan time.Time
}

func newGate() *gate {
	return &gate{asked: make(chan time.Duration, 100), release: make(chan time.Time, 100)}
}

func (g *gate) after(d time.Duration) <-chan time.Time { g.asked <- d; return g.release }

func (g *gate) await(t *testing.T) {
	t.Helper()
	select {
	case <-g.asked:
	case <-time.After(5 * time.Second):
		t.Fatal("the wait never went to sleep")
	}
}

func withAfter(f func(time.Duration) <-chan time.Time) func(*Relay) {
	return func(r *Relay) { r.After = f }
}

func row(id string, f func(map[string]any)) map[string]any {
	m := map[string]any{"id": id, "state": "running", "lastSeq": 5, "lastEventAt": "2026-10-20T10:00:00Z"}
	if f != nil {
		f(m)
	}
	return m
}

func waitCall(t *testing.T, s session, id any, args map[string]any, token any) {
	t.Helper()
	p := map[string]any{"name": toolRoomWait, "arguments": args}
	if token != nil {
		p["_meta"] = map[string]any{"progressToken": token}
	}
	s.send(t, map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call", "params": p})
}

// answer reads messages until the response to id, returning it and the messages before it.
func answer(t *testing.T, s session, id any) (map[string]any, []map[string]any) {
	t.Helper()
	var before []map[string]any
	for {
		m := s.next(t)
		if m["id"] == id && m["method"] == nil {
			return m, before
		}
		before = append(before, m)
	}
}

func resultOf(m map[string]any) (text string, sc map[string]any, isErr bool) {
	res := m["result"].(map[string]any)
	if c, _ := res["content"].([]any); len(c) > 0 {
		text, _ = c[0].(map[string]any)["text"].(string)
	}
	sc, _ = res["structuredContent"].(map[string]any)
	isErr, _ = res["isError"].(bool)
	return
}

func TestRoomWaitDefaultsToMyOpenRooms(t *testing.T) {
	b := newMCPBroker(t)
	b.roomText = "# card"
	var args []map[string]any
	b.roomsFn = func(n int, a map[string]any) []map[string]any {
		args = append(args, a)
		// 22 open rooms, r00 the newest; an ended room and the two oldest sort before r21 by id.
		var rows []map[string]any
		for i := range 22 {
			i := i
			rows = append(rows, row(fmt.Sprintf("aaaaaa%c%c", 'a'+i/10, 'a'+i%10), func(m map[string]any) {
				m["lastEventAt"] = time.Date(2026, 10, 20, 10, 60-i, 0, 0, time.UTC).Format(time.RFC3339)
				if n > 1 && (i == 0 || i == 20 || i == 21) {
					m["lastSeq"] = 9
				}
			}))
		}
		// A broker id that is not a room id is never watched, however fresh and busy.
		rows = append(rows, row("../etc", func(m map[string]any) {
			m["lastEventAt"] = "2026-10-20T12:00:00Z"
			if n > 1 {
				m["lastSeq"] = 9
			}
		}))
		rows = append(rows, row("bbbbbbbb", func(m map[string]any) {
			m["state"] = "done"
			m["lastEventAt"] = "2026-10-20T11:00:00Z"
			if n > 1 {
				m["lastSeq"] = 9
			}
		}))
		return rows
	}
	s := relayTo(t, b, tok, withAfter((&steps{}).after))
	waitCall(t, s, 1, map[string]any{"until": []string{"any_change"}}, nil)
	_, sc, isErr := resultOf(first(answer(t, s, 1.0)))
	if isErr || sc["room"] != "aaaaaaaa" || sc["condition"] != "any_change" {
		t.Fatalf("%v", sc)
	}
	if args[0]["mine"] != true {
		t.Fatalf("without rooms it asks for mine: %v", args[0])
	}
}

func first(m map[string]any, _ []map[string]any) map[string]any { return m }

func TestRoomWaitReturnsTheCard(t *testing.T) {
	b := newMCPBroker(t)
	b.roomText = "# card of aaaaaaaa"
	b.roomsFn = func(n int, _ map[string]any) []map[string]any {
		return []map[string]any{row("aaaaaaaa", func(m map[string]any) { m["needsYou"] = map[bool]int{false: 0, true: 1}[n >= 3] })}
	}
	st := &steps{}
	s := relayTo(t, b, tok, withAfter(st.after))
	waitCall(t, s, 1, map[string]any{"rooms": []string{"aaaaaaaa"}}, nil)
	text, sc, isErr := resultOf(first(answer(t, s, 1.0)))
	if isErr || text != "# card of aaaaaaaa" || sc["room"] != "aaaaaaaa" || sc["condition"] != "needs_you" {
		t.Fatalf("%q %v", text, sc)
	}
	if b.listed != 3 || len(st.waits()) != 2 {
		t.Fatalf("%d polls, %v", b.listed, st.waits())
	}
}

func TestRoomWaitBacksOff(t *testing.T) {
	b := newMCPBroker(t)
	b.roomText = "# card"
	b.roomsFn = func(n int, _ map[string]any) []map[string]any {
		return []map[string]any{row("aaaaaaaa", func(m map[string]any) {
			if n >= 8 {
				m["lastSeq"] = 6 // moves at the 8th poll: no hit, until is needs_you only
			}
			m["needsYou"] = map[bool]int{false: 0, true: 1}[n >= 10]
		})}
	}
	st := &steps{}
	s := relayTo(t, b, tok, withAfter(st.after))
	waitCall(t, s, 1, map[string]any{"rooms": []string{"aaaaaaaa"}, "until": []string{"needs_you"}}, nil)
	answer(t, s, 1.0)
	sec := func(f float64) time.Duration { return time.Duration(f * float64(time.Second)) }
	want := []time.Duration{sec(5), sec(7.5), sec(11.25), sec(16.875), sec(25.3125), sec(30), sec(30), sec(5), sec(7.5)}
	if got := st.waits(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("waits %v, want %v", got, want)
	}
}

func TestRoomWaitTimesOut(t *testing.T) {
	b := newMCPBroker(t)
	b.roomsFn = func(int, map[string]any) []map[string]any { return []map[string]any{row("aaaaaaaa", nil)} }
	st := &steps{}
	s := relayTo(t, b, tok, withAfter(st.after))
	waitCall(t, s, 1, map[string]any{"rooms": []string{"aaaaaaaa"}, "timeout": 12}, nil)
	text, sc, isErr := resultOf(first(answer(t, s, 1.0)))
	if isErr || !strings.Contains(text, "nothing new yet") || !strings.Contains(text, "aaaaaaaa") || sc["room"] != "" || sc["condition"] != "timeout" {
		t.Fatalf("%q %v", text, sc)
	}
	// The last wait is cut to what is left of the timeout, then one last poll.
	if got := st.waits(); fmt.Sprint(got) != fmt.Sprint([]time.Duration{5 * time.Second, 7 * time.Second}) || b.listed != 3 {
		t.Fatalf("%v, %d polls", got, b.listed)
	}
}

func TestRoomWaitProgressOnlyWithAToken(t *testing.T) {
	for name, tc := range map[string]struct {
		token any
		want  int
	}{"with a token": {"tk1", 3}, "without": {nil, 0}} {
		t.Run(name, func(t *testing.T) {
			b := newMCPBroker(t)
			b.roomText = "# card"
			b.roomsFn = func(n int, _ map[string]any) []map[string]any {
				return []map[string]any{row("aaaaaaaa", func(m map[string]any) { m["needsYou"] = map[bool]int{false: 0, true: 1}[n >= 4] })}
			}
			s := relayTo(t, b, tok, withAfter((&steps{}).after))
			waitCall(t, s, 1, map[string]any{"rooms": []string{"aaaaaaaa"}}, tc.token)
			_, before := answer(t, s, 1.0)
			n := 0
			for _, m := range before {
				if m["method"] == "notifications/progress" && m["params"].(map[string]any)["progressToken"] == tc.token {
					n++
				}
			}
			if n != tc.want || len(before) != tc.want {
				t.Fatalf("%d progress of %d messages: %v", n, len(before), before)
			}
		})
	}
}

func TestRoomWaitStopsOnCancel(t *testing.T) {
	b := newMCPBroker(t)
	b.roomsFn = func(int, map[string]any) []map[string]any { return []map[string]any{row("aaaaaaaa", nil)} }
	g := newGate()
	s := relayTo(t, b, tok, withAfter(g.after))
	waitCall(t, s, 7, map[string]any{"rooms": []string{"aaaaaaaa"}}, "tk")
	g.await(t)
	if m := s.next(t); m["method"] != "notifications/progress" { // the first poll's, before the cancel
		t.Fatalf("%v", m)
	}
	cancelOf(s, t, 7)
	g.release <- time.Now() // the next tick comes after the cancel
	// Closing stdin ends Serve only after the wait has returned: that is the sync point.
	_ = s.in.Close()
	for m := range s.lines {
		t.Fatalf("written after the cancel: %v", m)
	}
	if b.listed != 1 {
		t.Fatalf("%d polls: none after the cancel", b.listed)
	}
}

func TestRoomWaitStopsWhenStdinCloses(t *testing.T) {
	b := newMCPBroker(t)
	b.roomsFn = func(int, map[string]any) []map[string]any { return []map[string]any{row("aaaaaaaa", nil)} }
	g := newGate()
	s := relayTo(t, b, tok, withAfter(g.after))
	waitCall(t, s, 7, map[string]any{"rooms": []string{"aaaaaaaa"}}, "tk")
	g.await(t)
	if m := s.next(t); m["method"] != "notifications/progress" {
		t.Fatalf("%v", m)
	}
	_ = s.in.Close()
	select {
	case m, ok := <-s.lines: // the relay ends and closes its output; nothing is written first
		if ok {
			t.Fatalf("written after stdin closed: %v", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the wait outlived stdin")
	}
	if b.listed != 1 {
		t.Fatalf("%d polls", b.listed)
	}
}

func TestRoomWaitURLForm(t *testing.T) {
	const room = "aaaaaaaa"
	for name, tc := range map[string]struct {
		caps  map[string]any
		kind  string
		url   func(base string) string
		forms bool // the elicitation is sent
	}{
		"approval, rooms host, URL mode":   {map[string]any{"url": map[string]any{}}, "approval", func(b string) string { return b + "/r/" + room + "#ap1" }, true},
		"a query on the right host":        {map[string]any{"url": map[string]any{}}, "approval", func(b string) string { return b + "/r/" + room + "?next=https://evil.example#ap1" }, true},
		"URL mode with forms":              {map[string]any{"form": map[string]any{}, "url": map[string]any{}}, "approval", func(b string) string { return b + "/r/" + room + "#ap1" }, true},
		"another host":                     {map[string]any{"url": map[string]any{}}, "approval", func(string) string { return "https://evil.example/r/" + room + "#ap1" }, false},
		"another room's page":              {map[string]any{"url": map[string]any{}}, "approval", func(b string) string { return b + "/r/bbbbbbbb#ap1" }, false},
		"a userinfo trick":                 {map[string]any{"url": map[string]any{}}, "approval", func(b string) string { return strings.Replace(b, "://", "://evil.example@", 1) + "/r/" + room }, false},
		"a hold":                           {map[string]any{"url": map[string]any{}}, "hold", func(b string) string { return b + "/r/" + room + "#ap1" }, false},
		"client without URL mode":          {map[string]any{"form": map[string]any{}}, "approval", func(b string) string { return b + "/r/" + room + "#ap1" }, false},
		"client with the empty capability": {map[string]any{}, "approval", func(b string) string { return b + "/r/" + room + "#ap1" }, false},
	} {
		t.Run(name, func(t *testing.T) {
			b := newMCPBroker(t)
			link := tc.url(b.srv.URL)
			b.roomText = "# card"
			b.card = map[string]any{"room": room, "needsYou": []any{map[string]any{"kind": tc.kind, "id": "ap1", "what": "x", "url": link}}}
			b.roomsFn = func(n int, _ map[string]any) []map[string]any {
				return []map[string]any{row(room, func(m map[string]any) {
					if n >= 2 {
						m["needsMe"], m["needsYou"] = true, 1
						m["need"] = map[string]any{"kind": tc.kind, "id": "ap1", "url": link}
					}
				})}
			}
			s := relayTo(t, b, tok, withAfter((&steps{}).after))
			s.request(t, 1, "initialize", map[string]any{"capabilities": map[string]any{"elicitation": tc.caps}})
			waitCall(t, s, 2, map[string]any{"rooms": []string{room}}, nil)
			res, before := answer(t, s, 2.0)
			_, sc, _ := resultOf(res)
			if sc["condition"] != "needs_you" {
				t.Fatalf("%v", sc)
			}
			if !tc.forms {
				if len(before) != 0 {
					t.Fatalf("no elicitation expected: %v", before)
				}
				return
			}
			if len(before) != 1 || before[0]["method"] != "elicitation/create" {
				t.Fatalf("the URL form goes out before the result: %v", before)
			}
			p := before[0]["params"].(map[string]any)
			if p["mode"] != "url" || p["url"] != b.srv.URL+"/r/"+room+"#ap1" || p["elicitationId"] == "" {
				t.Fatalf("%v", p)
			}
		})
	}
}

func TestRoomWaitRefusesBadArguments(t *testing.T) {
	s := relayTo(t, newMCPBroker(t), tok)
	var many []string
	for i := range 21 {
		many = append(many, fmt.Sprintf("aaaaaaa%c", 'a'+i%20))
	}
	for name, tc := range map[string]struct {
		args map[string]any
		want string
	}{
		"21 rooms":            {map[string]any{"rooms": many}, "at most 20 rooms"},
		"a bad room id":       {map[string]any{"rooms": []string{"../etc"}}, "invalid_arguments"},
		"a bad condition":     {map[string]any{"until": []string{"soon"}}, "invalid_arguments"},
		"a timeout too long":  {map[string]any{"timeout": 1801}, "invalid_arguments"},
		"a timeout too short": {map[string]any{"timeout": 0}, "invalid_arguments"},
		"an unknown argument": {map[string]any{"after": "x"}, "invalid_arguments"},
	} {
		t.Run(name, func(t *testing.T) {
			waitCall(t, s, 1, tc.args, nil)
			text, _, isErr := resultOf(first(answer(t, s, 1.0)))
			if !isErr || !strings.Contains(text, tc.want) {
				t.Fatalf("%v %q", isErr, text)
			}
		})
	}
}

func TestRoomWaitUnknownRoom(t *testing.T) {
	b := newMCPBroker(t)
	b.roomsFn = func(int, map[string]any) []map[string]any { return []map[string]any{row("aaaaaaaa", nil)} }
	s := relayTo(t, b, tok, withAfter((&steps{}).after))
	waitCall(t, s, 1, map[string]any{"rooms": []string{"aaaaaaaa", "bbbbbbbb"}}, nil)
	text, _, isErr := resultOf(first(answer(t, s, 1.0)))
	if !isErr || !strings.Contains(text, "bbbbbbbb") {
		t.Fatalf("%v %q", isErr, text)
	}
}

func TestRoomWaitGivesUpAfterFailedPolls(t *testing.T) {
	b := newMCPBroker(t)
	b.status = 500
	st := &steps{}
	s := relayTo(t, b, tok, withAfter(st.after))
	waitCall(t, s, 1, map[string]any{"rooms": []string{"aaaaaaaa"}}, nil)
	text, _, isErr := resultOf(first(answer(t, s, 1.0)))
	if !isErr || !strings.HasPrefix(text, "not_watching:") || len(st.waits()) != 12 { // 60 s of 5 s waits
		t.Fatalf("%q %v", text, st.waits())
	}
}

func TestRoomWaitLeavesSlots(t *testing.T) {
	b := newMCPBroker(t)
	b.rooms = []map[string]any{row("aaaaaaaa", nil)}
	// The rooms_list the waits poll and the one asked for below are the same call.
	g := newGate()
	s := relayTo(t, b, tok, withAfter(g.after))
	for i := 1; i <= maxWaits; i++ {
		waitCall(t, s, i, map[string]any{"rooms": []string{"aaaaaaaa"}}, nil)
	}
	for range maxWaits {
		g.await(t)
	}
	// Every in-flight slot is free for other requests while 8 waits block.
	for i := range 10 {
		res := s.request(t, 100+i, "tools/call", map[string]any{"name": "rooms_list", "arguments": map[string]any{}})
		if res["id"] != float64(100+i) || res["error"] != nil {
			t.Fatalf("a request starved by the waits: %v", res)
		}
	}
	// A ninth wait is refused, not queued.
	waitCall(t, s, 50, map[string]any{"rooms": []string{"aaaaaaaa"}}, nil)
	if res := s.next(t); res["id"] != 50.0 || !strings.Contains(fmt.Sprint(res["error"]), "busy") {
		t.Fatalf("%v", res)
	}
	// A cancellation still ends one of them, which frees its place.
	cancelOf(s, t, 3)
	deadline := time.After(5 * time.Second)
	for id := 60; ; id++ {
		waitCall(t, s, id, map[string]any{"rooms": []string{"aaaaaaaa"}}, nil)
		select {
		case <-g.asked: // accepted, and polling
			return
		case m := <-s.lines:
			if !strings.Contains(fmt.Sprint(m["error"]), "busy") {
				t.Fatalf("%v", m)
			}
		case <-deadline:
			t.Fatal("the cancelled wait did not end")
		}
	}
}

func TestRoomWaitSchema(t *testing.T) {
	var sch struct {
		AdditionalProperties bool `json:"additionalProperties"`
		Properties           struct {
			Rooms struct {
				MaxItems int `json:"maxItems"`
			} `json:"rooms"`
			Until struct {
				Items struct {
					Enum []string `json:"enum"`
				} `json:"items"`
			} `json:"until"`
			Timeout struct{ Minimum, Maximum int } `json:"timeout"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(roomWaitEntry["inputSchema"].(json.RawMessage), &sch); err != nil {
		t.Fatal(err)
	}
	p := sch.Properties
	if sch.AdditionalProperties || p.Rooms.MaxItems != 20 || strings.Join(p.Until.Items.Enum, ",") != "needs_you,pr_opened,ended,any_change" ||
		p.Timeout.Minimum != 1 || p.Timeout.Maximum != 1800 {
		t.Fatalf("%+v", sch)
	}
	if d := roomWaitEntry["description"].(string); strings.Contains(d, "Not yet available") || !strings.Contains(d, "1800") {
		t.Fatalf("%q", d)
	}
}

func TestRoomWaitDefaultTimeoutIs300Seconds(t *testing.T) {
	b := newMCPBroker(t)
	b.roomsFn = func(int, map[string]any) []map[string]any { return []map[string]any{row("aaaaaaaa", nil)} }
	st := &steps{}
	s := relayTo(t, b, tok, withAfter(st.after))
	waitCall(t, s, 1, map[string]any{}, nil)
	text, sc, isErr := resultOf(first(answer(t, s, 1.0)))
	var total time.Duration
	for _, w := range st.waits() {
		total += w
	}
	if isErr || sc["condition"] != "timeout" || total != 300*time.Second || !strings.Contains(text, "nothing new yet") {
		t.Fatalf("%v %q waited %v", sc, text, total)
	}
}

func TestRoomWaitWithNoOpenRoomsSaysNone(t *testing.T) {
	b := newMCPBroker(t)
	b.roomsFn = func(int, map[string]any) []map[string]any { return nil }
	s := relayTo(t, b, tok, withAfter((&steps{}).after))
	waitCall(t, s, 1, map[string]any{}, nil)
	text, sc, isErr := resultOf(first(answer(t, s, 1.0)))
	if isErr || sc["condition"] != "none" || !strings.Contains(text, "no open rooms") {
		t.Fatalf("%v %q", sc, text)
	}
}

func TestRoomWaitTimeoutAfterFailedPollsSaysSo(t *testing.T) {
	b := newMCPBroker(t)
	b.status = 500
	s := relayTo(t, b, tok, withAfter((&steps{}).after))
	waitCall(t, s, 1, map[string]any{"rooms": []string{"aaaaaaaa"}, "timeout": 1}, nil)
	text, _, _ := resultOf(first(answer(t, s, 1.0)))
	if !strings.Contains(text, "could not reach the broker") || strings.Contains(text, "nothing new") {
		t.Fatalf("%q", text)
	}
}
