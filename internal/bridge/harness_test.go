// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Smana/agent-platform/internal/httpx"
)

const conv = "0b3c6f0e-5d1a-4b8e-9f41-2a7c3e9d8b10"

// bounded is the test's context with a deadline, so a walk that loops fails the
// test in seconds instead of hanging it until go test's timeout.
func bounded(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func agentEvents(f *fakeAgentServer, n int) {
	for range n {
		f.add(map[string]any{"kind": "MessageEvent", "source": "agent"})
	}
}

func ids(evs []RawEvent) []string {
	out := make([]string, 0, len(evs))
	for _, e := range evs {
		out = append(out, e.ID)
	}
	return out
}

func TestNextWalksPagesWithoutRepeatsOrGaps(t *testing.T) {
	ctx := bounded(t)
	f := &fakeAgentServer{pageSize: 3, status: "running"}
	agentEvents(f, 7)
	h := NewHarness(f.start(t, conv).URL, conv)
	h.MaxPages = 10
	evs, cur, err := h.Next(ctx, Cursor{})
	if err != nil || len(evs) != 7 || cur.Count != 7 || cur.LastID != "e7" {
		t.Fatalf("first walk: %d %+v %v", len(evs), cur, err)
	}
	evs, cur, err = h.Next(ctx, cur)
	if err != nil || len(evs) != 0 || cur.Count != 7 {
		t.Fatalf("an idle poll returned %d events, %v", len(evs), err)
	}
	f.add(map[string]any{"kind": "ActionEvent", "source": "agent"})
	evs, cur, err = h.Next(ctx, cur)
	if err != nil || len(evs) != 1 || evs[0].ID != "e8" || cur.Count != 8 {
		t.Fatalf("the page_id is inclusive: got %d events, cursor %+v, %v", len(evs), cur, err)
	}
	if evs[0].Kind != "ActionEvent" || evs[0].Source != "agent" || !strings.Contains(string(evs[0].Raw), `"id":"e8"`) {
		t.Fatalf("the event is kept whole: %+v", evs[0])
	}
}

func TestSkipResumesAfterARestart(t *testing.T) {
	cases := []struct {
		name   string
		events int
		skip   int64
		want   Cursor
		after  []string
	}{
		{"it stops after the nth event", 5, 3, Cursor{LastID: "e3", Count: 3}, []string{"e4", "e5"}},
		{"a log shorter than n leaves the cursor after its last event", 5, 10, Cursor{LastID: "e5", Count: 5}, []string{}},
		{"zero is a fresh cursor", 5, 0, Cursor{}, []string{"e1", "e2", "e3", "e4", "e5"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := bounded(t)
			f := &fakeAgentServer{pageSize: 2, status: "running"}
			agentEvents(f, c.events)
			h := NewHarness(f.start(t, conv).URL, conv)
			h.MaxPages = 10
			cur, err := h.Skip(ctx, c.skip)
			if err != nil || cur != c.want {
				t.Fatalf("skip: %+v %v", cur, err)
			}
			evs, _, err := h.Next(ctx, cur)
			if err != nil || !slices.Equal(ids(evs), c.after) {
				t.Fatalf("after skip: %v, %v", ids(evs), err)
			}
		})
	}
}

// Review M13 and F6: a backlog is read a few pages per poll, the default is
// bounded, and nothing is lost between polls.
func TestNextStopsAfterMaxPages(t *testing.T) {
	cases := []struct {
		name     string
		maxPages int
		polls    [][]string
	}{
		{"two pages per poll", 2, [][]string{{"e1", "e2", "e3", "e4"}, {"e5"}}},
		// page_id is inclusive: a page after the first starts with the cursor's own event.
		{"no cap is not an option: below one reads one page", 0, [][]string{{"e1", "e2"}, {"e3"}, {"e4"}, {"e5"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := bounded(t)
			f := &fakeAgentServer{pageSize: 2, status: "running"}
			agentEvents(f, 5)
			h := NewHarness(f.start(t, conv).URL, conv)
			h.MaxPages = c.maxPages
			var cur Cursor
			for i, want := range c.polls {
				evs, next, err := h.Next(ctx, cur)
				if err != nil || !slices.Equal(ids(evs), want) || next.Count != cur.Count+int64(len(want)) {
					t.Fatalf("poll %d: %v %+v %v", i, ids(evs), next, err)
				}
				cur = next
			}
		})
	}
	if h := NewHarness("http://127.0.0.1:1", conv); h.MaxPages != DefaultMaxPages || DefaultMaxPages < 1 {
		t.Fatalf("NewHarness leaves MaxPages at %d", h.MaxPages)
	}
}

// Review F1 and F3: a malformed event is returned once and counted, so seqs stay
// aligned, but never keys the cursor: the cursor neither rewinds nor wedges.
func TestMalformedEventsNeverKeyTheCursor(t *testing.T) {
	ok := func() map[string]any { return map[string]any{"kind": "MessageEvent", "source": "agent"} }
	noID := func() map[string]any { return map[string]any{"kind": "MessageEvent"} }
	numericID := func() map[string]any { return map[string]any{"id": 42, "kind": "MessageEvent"} }
	cases := []struct {
		name  string
		setup func(f *fakeAgentServer)
		want  Cursor
		later map[string]any
		after []string
	}{
		{
			name:  "an id-less last event is returned once, then an idle poll returns nothing",
			setup: func(f *fakeAgentServer) { f.add(ok()); f.addAsIs(noID()) },
			want:  Cursor{LastID: "e1", Count: 2, Unkeyed: 1},
		},
		{
			name:  "a non-string id is returned once, then an idle poll returns nothing",
			setup: func(f *fakeAgentServer) { f.add(ok()); f.addAsIs(numericID()) },
			want:  Cursor{LastID: "e1", Count: 2, Unkeyed: 1},
		},
		{
			name:  "a malformed event between two is passed and the next one keys the cursor",
			setup: func(f *fakeAgentServer) { f.add(ok()); f.addAsIs(noID()); f.add(ok()) },
			want:  Cursor{LastID: "e3", Count: 3},
		},
		{
			name:  "a log that starts malformed resumes from its start, passing what it returned",
			setup: func(f *fakeAgentServer) { f.addAsIs(noID()) },
			want:  Cursor{Count: 1, Unkeyed: 1},
			later: ok(),
			after: []string{"e2"},
		},
		{
			name:  "an event appended after a trailing malformed one is read once",
			setup: func(f *fakeAgentServer) { f.add(ok()); f.addAsIs(noID()); f.addAsIs(noID()) },
			want:  Cursor{LastID: "e1", Count: 3, Unkeyed: 2},
			later: ok(),
			after: []string{"e4"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := bounded(t)
			f := &fakeAgentServer{pageSize: 100, status: "running"}
			c.setup(f)
			h := NewHarness(f.start(t, conv).URL, conv)
			evs, cur, err := h.Next(ctx, Cursor{})
			if err != nil || cur != c.want || int64(len(evs)) != c.want.Count {
				t.Fatalf("first poll: %d events, %+v, %v", len(evs), cur, err)
			}
			for _, e := range evs {
				if e.Malformed == (e.ID != "" && strings.HasPrefix(e.ID, "e")) {
					t.Errorf("%q decoded with Malformed=%v", e.ID, e.Malformed)
				}
			}
			again, still, err := h.Next(ctx, cur)
			if err != nil || len(again) != 0 || still != cur {
				t.Fatalf("an idle poll returned %d events, %+v, %v", len(again), still, err)
			}
			if c.later == nil {
				return
			}
			f.add(c.later)
			evs, next, err := h.Next(ctx, cur)
			if err != nil || !slices.Equal(ids(evs), c.after) || next.Count != cur.Count+1 || next.Unkeyed != 0 {
				t.Fatalf("after a new event: %v %+v %v", ids(evs), next, err)
			}
		})
	}
	t.Run("skip lands where next did", func(t *testing.T) {
		ctx := bounded(t)
		f := &fakeAgentServer{pageSize: 2, status: "running"}
		f.add(ok())
		f.addAsIs(noID())
		f.add(ok())
		h := NewHarness(f.start(t, conv).URL, conv)
		cur, err := h.Skip(ctx, 2)
		if err != nil || cur != (Cursor{LastID: "e1", Count: 2, Unkeyed: 1}) {
			t.Fatalf("skip: %+v %v", cur, err)
		}
		evs, _, err := h.Next(ctx, cur)
		if err != nil || !slices.Equal(ids(evs), []string{"e3"}) {
			t.Fatalf("after skip: %v %v", ids(evs), err)
		}
	})
}

func TestRawEventDecodesEveryEventTolerantly(t *testing.T) {
	cases := []struct {
		name      string
		in        string
		id, kind  string
		malformed bool
	}{
		{"a well-formed event", `{"id":"e1","kind":"ActionEvent","source":"agent","x":1}`, "e1", "ActionEvent", false},
		{"no id", `{"kind":"ActionEvent"}`, "", "ActionEvent", true},
		{"an empty id", `{"id":"","kind":"ActionEvent"}`, "", "ActionEvent", true},
		{"a numeric id", `{"id":42,"kind":"ActionEvent"}`, "", "ActionEvent", true},
		{"a numeric kind", `{"id":"e1","kind":7}`, "e1", "", true},
		{"a null source", `{"id":"e1","kind":"ActionEvent","source":null}`, "e1", "ActionEvent", true},
		{"not an object", `[1,2]`, "", "", true},
		{"null", `null`, "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var page struct{ Items []RawEvent }
			if err := json.Unmarshal([]byte(`{"items":[`+c.in+`]}`), &page); err != nil || len(page.Items) != 1 {
				t.Fatalf("a malformed event failed the whole page: %v", err)
			}
			e := page.Items[0]
			if e.ID != c.id || e.Kind != c.kind || e.Malformed != c.malformed || string(e.Raw) != c.in {
				t.Fatalf("got %+v", e)
			}
		})
	}
}

// Review F3: a page over the byte cap is asked again with half the limit; only
// one event alone over it stops the walk, loudly and without moving past it.
func TestPagesOverTheCapAreHalved(t *testing.T) {
	t.Run("a page over the cap is read in smaller pages, nothing lost", func(t *testing.T) {
		ctx := bounded(t)
		f := &fakeAgentServer{pageSize: 100, status: "running"}
		for range 10 {
			f.add(map[string]any{"kind": "MessageEvent", "text": strings.Repeat("x", 1<<10)})
		}
		h := NewHarness(f.start(t, conv).URL, conv)
		h.maxBody, h.MaxPages = 4<<10, 100
		evs, cur, err := h.Next(ctx, Cursor{})
		if err != nil || len(evs) != 10 || cur.LastID != "e10" {
			t.Fatalf("%d events, %+v, %v", len(evs), cur, err)
		}
		if got := f.asked(); len(got) < 6 || !slices.Equal(got[:6], []int{100, 50, 25, 12, 6, 3}) {
			t.Fatalf("limits asked: %v", got)
		}
	})
	t.Run("a page that only repeats the cursor's event does not use up the poll", func(t *testing.T) {
		ctx := bounded(t)
		f := &fakeAgentServer{pageSize: 100, status: "running"}
		for range 5 {
			f.add(map[string]any{"kind": "MessageEvent", "text": strings.Repeat("x", 1<<10)})
		}
		h := NewHarness(f.start(t, conv).URL, conv)
		h.maxBody = 1500 // one event per page: the page from LastID holds LastID alone
		var got []string
		var cur Cursor
		for range 10 {
			evs, next, err := h.Next(ctx, cur)
			if err != nil {
				t.Fatal(err)
			}
			got, cur = append(got, ids(evs)...), next
		}
		if !slices.Equal(got, []string{"e1", "e2", "e3", "e4", "e5"}) {
			t.Fatalf("ten polls read %v", got)
		}
	})
	t.Run("one event alone over the cap is ErrEventTooLarge, and never skipped", func(t *testing.T) {
		ctx := bounded(t)
		f := &fakeAgentServer{pageSize: 100, status: "running"}
		f.add(map[string]any{"kind": "MessageEvent"})
		f.add(map[string]any{"kind": "MessageEvent", "text": strings.Repeat("x", 8<<10)})
		f.add(map[string]any{"kind": "MessageEvent"})
		h := NewHarness(f.start(t, conv).URL, conv)
		h.maxBody, h.MaxPages = 4<<10, 100
		evs, cur, err := h.Next(ctx, Cursor{})
		if !errors.Is(err, ErrEventTooLarge) || !errors.Is(err, httpx.ErrBodyTooLarge) ||
			!slices.Equal(ids(evs), []string{"e1"}) || cur != (Cursor{LastID: "e1", Count: 1}) {
			t.Fatalf("%v %+v %v", ids(evs), cur, err)
		}
		evs, still, err := h.Next(ctx, cur)
		if !errors.Is(err, ErrEventTooLarge) || len(evs) != 0 || still != cur {
			t.Fatalf("the walk moved past the event: %v %+v %v", ids(evs), still, err)
		}
		if _, err := h.Skip(ctx, 3); !errors.Is(err, ErrEventTooLarge) {
			t.Fatalf("skip: %v", err)
		}
	})
}

// A misbehaving agent-server fails the call with an error the bridge can branch
// on, or ends the walk; it never hangs a poll, repeats events or fills the
// sidecar's memory.
func TestHarnessRefusesWhatAgentServerMustNotSend(t *testing.T) {
	page := func(body string) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }
	}
	cases := []struct {
		name    string
		handler http.HandlerFunc
		call    func(context.Context, *Harness) error
		want    func(error) bool
	}{
		{
			name:    "a conversation agent-run has not created yet is a StatusError with its code",
			handler: func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "not found", http.StatusNotFound) },
			call:    func(ctx context.Context, h *Harness) error { return h.Ready(ctx) },
			want: func(err error) bool {
				se, ok := errors.AsType[*StatusError](err)
				return ok && se.Code == http.StatusNotFound
			},
		},
		{
			name:    "a 2xx other than 200 is success",
			handler: func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusAccepted) },
			call:    func(ctx context.Context, h *Harness) error { return h.Send(ctx, "hi") },
			want:    func(err error) bool { return err == nil },
		},
		{
			name:    "an empty page that names a next page ends the walk instead of looping",
			handler: page(`{"items":[],"next_page_id":"e9"}`),
			call: func(ctx context.Context, h *Harness) error {
				evs, cur, err := h.Next(ctx, Cursor{})
				if err == nil && (len(evs) != 0 || cur != (Cursor{})) {
					return errors.New("the cursor moved over nothing")
				}
				_, err = h.Skip(ctx, 3)
				return err
			},
			want: func(err error) bool { return err == nil },
		},
		{
			name:    "a page whose next page is itself ends the walk instead of looping",
			handler: page(`{"items":[{"id":"e1","kind":"MessageEvent"}],"next_page_id":"e1"}`),
			call: func(ctx context.Context, h *Harness) error {
				evs, cur, err := h.Next(ctx, Cursor{})
				if err == nil && (len(evs) != 1 || cur != (Cursor{LastID: "e1", Count: 1})) {
					return errors.New("the walk repeated a page")
				}
				_, err = h.Skip(ctx, 3)
				return err
			},
			want: func(err error) bool { return err == nil },
		},
		{
			name: "a next page already returned ends the walk instead of looping",
			handler: func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("page_id") == "" {
					_, _ = w.Write([]byte(`{"items":[{"id":"e1"},{"id":"e2"}],"next_page_id":"e3"}`))
					return
				}
				_, _ = w.Write([]byte(`{"items":[{"id":"e3"}],"next_page_id":"e1"}`))
			},
			call: func(ctx context.Context, h *Harness) error {
				evs, cur, err := h.Next(ctx, Cursor{})
				if err == nil && (!slices.Equal(ids(evs), []string{"e1", "e2", "e3"}) || cur.Count != 3) {
					return errors.New("the walk went back over returned events")
				}
				_, err = h.Skip(ctx, 9)
				return err
			},
			want: func(err error) bool { return err == nil },
		},
		{
			name:    "a next page id that is not a string ends the walk, not the page",
			handler: page(`{"items":[{"id":"e1"}],"next_page_id":42}`),
			call: func(ctx context.Context, h *Harness) error {
				evs, _, err := h.Next(ctx, Cursor{})
				if err == nil && len(evs) != 1 {
					return errors.New("the page was lost")
				}
				return err
			},
			want: func(err error) bool { return err == nil },
		},
		{
			name:    "a server that ignores page_id is ErrCursorLost, not the log again",
			handler: page(`{"items":[{"id":"e1"},{"id":"e2"}],"next_page_id":null}`),
			call: func(ctx context.Context, h *Harness) error {
				evs, cur, err := h.Next(ctx, Cursor{LastID: "e2", Count: 2})
				if len(evs) != 0 || cur != (Cursor{LastID: "e2", Count: 2}) {
					return errors.New("the cursor moved")
				}
				return err
			},
			want: func(err error) bool { return errors.Is(err, ErrCursorLost) },
		},
		{
			name:    "an empty answer to the cursor's own page is ErrCursorLost",
			handler: page(`{"items":[],"next_page_id":null}`),
			call: func(ctx context.Context, h *Harness) error {
				_, _, err := h.Next(ctx, Cursor{LastID: "e2", Count: 2})
				return err
			},
			want: func(err error) bool { return errors.Is(err, ErrCursorLost) },
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(c.handler)
			t.Cleanup(srv.Close)
			h := NewHarness(srv.URL, conv)
			h.MaxPages = 1 << 20
			if err := c.call(bounded(t), h); !c.want(err) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

// The requests themselves: the conversation id is one path segment, and only a
// request with a body says it is JSON.
func TestRequestsAreWellFormed(t *testing.T) {
	cases := []struct {
		name     string
		call     func(context.Context, *Harness) error
		path     string
		wantType string
	}{
		{"a write with a body is JSON", func(ctx context.Context, h *Harness) error { return h.Send(ctx, "hi") },
			"/api/conversations/a%2Fb%3F/events", "application/json"},
		{"a write without a body has no Content-Type", func(ctx context.Context, h *Harness) error { return h.Interrupt(ctx) },
			"/api/conversations/a%2Fb%3F/interrupt", ""},
		{"a read has no Content-Type", func(ctx context.Context, h *Harness) error { return h.Ready(ctx) },
			"/api/conversations/a%2Fb%3F", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var path, ctype string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				path, ctype = r.URL.EscapedPath(), r.Header.Get("Content-Type")
				_, _ = w.Write([]byte(`{}`))
			}))
			t.Cleanup(srv.Close)
			if err := c.call(bounded(t), NewHarness(srv.URL, "a/b?")); err != nil {
				t.Fatal(err)
			}
			if path != c.path || ctype != c.wantType {
				t.Fatalf("path %q, Content-Type %q", path, ctype)
			}
		})
	}
}

func TestWritesReachAgentServer(t *testing.T) {
	f := &fakeAgentServer{pageSize: 100, status: "waiting_for_confirmation"}
	h := NewHarness(f.start(t, conv).URL, conv)
	ctx := bounded(t)
	if err := h.Ready(ctx); err != nil {
		t.Fatalf("ready: %v", err)
	}
	if err := h.Send(ctx, "use the v2 API"); err != nil {
		t.Fatalf("send: %v", err)
	}
	if err := h.Respond(ctx, false, "denied by policy"); err != nil {
		t.Fatalf("respond: %v", err)
	}
	if err := h.AlwaysConfirm(ctx); err != nil {
		t.Fatalf("policy: %v", err)
	}
	sent, responses, policy := f.snapshot()
	if len(sent) != 1 || sent[0] != "use the v2 API" || len(responses) != 1 || responses[0] || policy != "AlwaysConfirm" {
		t.Fatalf("sent=%v responses=%v policy=%q", sent, responses, policy)
	}
	if err := h.Interrupt(ctx); err != nil {
		t.Fatal(err)
	}
	if s, err := h.Status(ctx); err != nil || s != "paused" {
		t.Fatalf("status %q, %v", s, err)
	}
}
