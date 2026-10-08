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
	"github.com/Smana/agent-platform/internal/runwatch"
)

func TestProgressIsAOneLineNote(t *testing.T) {
	log := &memLog{}
	out, err := tool(tools(log), "room_progress").Call(t.Context(), impl, json.RawMessage(`{"text":"edited line 12, running checks"}`))
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := json.Marshal(out); string(b) != `{"seq":1}` {
		t.Fatalf("result = %s", b)
	}
	if got := string(log.drafts[0].Payload); got != `{"kind":"progress","text":"edited line 12, running checks","delivery":"none"}` {
		t.Fatalf("payload = %s", got)
	}
}

func TestProgressCountsCharactersNotBytes(t *testing.T) {
	for name, tc := range map[string]struct {
		text string
		ok   bool
	}{
		"280 characters":            {strings.Repeat("a", 280), true},
		"281 characters":            {strings.Repeat("a", 281), false},
		"280 runes of 3 bytes each": {strings.Repeat("日", 280), true},
		"281 runes of 3 bytes each": {strings.Repeat("日", 281), false},
		"a newline":                 {"two\nlines", false},
		"a carriage return":         {"two\rlines", false},
		"a control character":       {"a\u001b[31mb", false},
		"blank":                     {"  ", false},
		"empty":                     {"", false},
		"a tab":                     {"a\tb", true},
	} {
		t.Run(name, func(t *testing.T) {
			log := &memLog{}
			args, _ := json.Marshal(map[string]string{"text": tc.text})
			_, err := tool(tools(log), "room_progress").Call(t.Context(), impl, args)
			var bad argError
			switch {
			case tc.ok && err != nil:
				t.Fatalf("refused: %v", err)
			case !tc.ok && (!errors.As(err, &bad) || !strings.Contains(err.Error(), "1 to 280 characters")):
				t.Fatalf("err = %v, want an argument error", err)
			case !tc.ok && len(log.drafts) != 0:
				t.Fatal("stored a refused note")
			}
		})
	}
}

func TestProgressIsOneAMinutePerRun(t *testing.T) {
	log := &memLog{}
	c := newClock()
	progress := tool(RoomTools(log, fakeRedactor{}, c.now), "room_progress")
	note := func(run Caller, text string) error {
		_, err := progress.Call(t.Context(), run, json.RawMessage(`{"text":"`+text+`"}`))
		return err
	}
	if err := note(impl, "one"); err != nil {
		t.Fatal(err)
	}
	c.add(59 * time.Second)
	var slow rateError
	if err := note(impl, "two"); !errors.As(err, &slow) || !strings.Contains(err.Error(), "one progress note a minute") {
		t.Fatalf("second note within a minute: %v", err)
	}
	other := Caller{Run: runwatch.Run{ID: "aaaaaaaa", Room: "3kq7x2ma", Role: "tester"}}
	if err := note(other, "elsewhere"); err != nil {
		t.Fatalf("another run is not limited by this one: %v", err)
	}
	c.add(2 * time.Second)
	if err := note(impl, "three"); err != nil {
		t.Fatal(err)
	}
	if len(log.drafts) != 3 {
		t.Fatalf("%d appends, want 3", len(log.drafts))
	}
}

// The server shows the model the tool's own limit, not a generic log failure.
func TestProgressRateLimitIsNamedBehindTheServer(t *testing.T) {
	c := newClock()
	s, rejected := testServer(watcher(t, "implementer"), c)
	s.Tools = RoomTools(&memLog{}, fakeRedactor{}, c.now)
	callWith(t, s, "room_progress", map[string]any{"text": "one"})
	c.add(2 * time.Second) // clear the server's one call a second
	res, text := callWith(t, s, "room_progress", map[string]any{"text": "two"})
	if res["isError"] != true || text != "rate_limited: one progress note a minute" || len(*rejected) != 1 || (*rejected)[0] != "rate_limited" {
		t.Fatalf("%v %q %v", res, text, *rejected)
	}
}

// flaky fails its first append.
type flaky struct {
	memLog
	failed bool
}

func (f *flaky) Append(ctx context.Context, d envelope.Draft) (envelope.Event, bool, error) {
	if !f.failed {
		f.failed = true
		return envelope.Event{}, false, errors.New("db down")
	}
	return f.memLog.Append(ctx, d)
}

func TestProgressNotStoredDoesNotCostTheMinute(t *testing.T) {
	progress := tool(RoomTools(&flaky{}, fakeRedactor{}, newClock().now), "room_progress")
	args := json.RawMessage(`{"text":"one"}`)
	if _, err := progress.Call(t.Context(), impl, args); err == nil {
		t.Fatal("the first append should fail")
	}
	if _, err := progress.Call(t.Context(), impl, args); err != nil {
		t.Fatalf("retry within the minute: %v", err)
	}
}

func TestNoteGateIsBounded(t *testing.T) {
	g := &noteGate{}
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	for i := range maxNoteRuns {
		if !g.allow(fmt.Sprint(i), now) {
			t.Fatalf("run %d refused below the cap", i)
		}
	}
	if g.allow("one-too-many", now.Add(30*time.Second)) {
		t.Fatal("admitted a run past the cap while every run is recent")
	}
	if !g.allow("one-too-many", now.Add(61*time.Second)) {
		t.Fatal("quiet runs should be dropped to make room")
	}
	if len(g.last) > maxNoteRuns {
		t.Fatalf("%d tracked runs", len(g.last))
	}
}
