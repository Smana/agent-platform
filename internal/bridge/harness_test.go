// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Smana/agent-platform/internal/httpx"
)

const conv = "0b3c6f0e-5d1a-4b8e-9f41-2a7c3e9d8b10"

func agentEvents(f *fakeAgentServer, n int) {
	for range n {
		f.add(map[string]any{"kind": "MessageEvent", "source": "agent"})
	}
}

func TestNextWalksPagesWithoutRepeatsOrGaps(t *testing.T) {
	f := &fakeAgentServer{pageSize: 3, status: "running"}
	agentEvents(f, 7)
	h := NewHarness(f.start(t, conv).URL, conv)
	evs, cur, err := h.Next(t.Context(), Cursor{})
	if err != nil || len(evs) != 7 || cur.Count != 7 || cur.LastID != "e7" {
		t.Fatalf("first walk: %d %+v %v", len(evs), cur, err)
	}
	evs, cur, err = h.Next(t.Context(), cur)
	if err != nil || len(evs) != 0 || cur.Count != 7 {
		t.Fatalf("an idle poll returned %d events, %v", len(evs), err)
	}
	f.add(map[string]any{"kind": "ActionEvent", "source": "agent"})
	evs, cur, err = h.Next(t.Context(), cur)
	if err != nil || len(evs) != 1 || evs[0].ID != "e8" || cur.Count != 8 {
		t.Fatalf("the page_id is inclusive: got %d events, cursor %+v, %v", len(evs), cur, err)
	}
	if evs[0].Kind != "ActionEvent" || evs[0].Source != "agent" || !strings.Contains(string(evs[0].Raw), `"id":"e8"`) {
		t.Fatalf("the event is kept whole: %+v", evs[0])
	}
}

func TestSkipResumesAfterARestart(t *testing.T) {
	f := &fakeAgentServer{pageSize: 2, status: "running"}
	agentEvents(f, 5)
	h := NewHarness(f.start(t, conv).URL, conv)
	cur, err := h.Skip(t.Context(), 3)
	if err != nil || cur.Count != 3 || cur.LastID != "e3" {
		t.Fatalf("%+v %v", cur, err)
	}
	evs, _, err := h.Next(t.Context(), cur)
	if err != nil || len(evs) != 2 || evs[0].ID != "e4" {
		t.Fatalf("after skip: %d events, %v", len(evs), err)
	}
}

// Review M13: a backlog is read a few pages per poll, and nothing is lost between polls.
func TestNextStopsAfterMaxPages(t *testing.T) {
	f := &fakeAgentServer{pageSize: 2, status: "running"}
	agentEvents(f, 5)
	h := NewHarness(f.start(t, conv).URL, conv)
	h.MaxPages = 2
	evs, cur, err := h.Next(t.Context(), Cursor{})
	if err != nil || len(evs) != 4 || cur.LastID != "e4" {
		t.Fatalf("first poll: %d %+v %v", len(evs), cur, err)
	}
	evs, cur, err = h.Next(t.Context(), cur)
	if err != nil || len(evs) != 1 || cur.Count != 5 {
		t.Fatalf("second poll: %d %+v %v", len(evs), cur, err)
	}
}

func TestWritesReachAgentServer(t *testing.T) {
	f := &fakeAgentServer{pageSize: 100, status: "waiting_for_confirmation"}
	h := NewHarness(f.start(t, conv).URL, conv)
	ctx := t.Context()
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

// A misbehaving agent-server fails the call with an error the bridge can branch
// on; it never hangs a poll or fills the sidecar's memory.
func TestHarnessRefusesWhatAgentServerMustNotSend(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		call    func(*Harness) error
		want    func(error) bool
	}{
		{
			name: "a conversation agent-run has not created yet is a StatusError with its code",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "not found", http.StatusNotFound)
			},
			call: func(h *Harness) error { return h.Ready(t.Context()) },
			want: func(err error) bool {
				se, ok := errors.AsType[*StatusError](err)
				return ok && se.Code == http.StatusNotFound
			},
		},
		{
			name: "a page larger than the cap is refused, not read",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"items":[{"id":"e1","text":"` + strings.Repeat("x", maxResponseBytes) + `"}]}`))
			},
			call: func(h *Harness) error { _, _, err := h.Page(t.Context(), ""); return err },
			want: func(err error) bool { return errors.Is(err, httpx.ErrBodyTooLarge) },
		},
		{
			name: "an empty page that names a next page ends the walk instead of looping",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"items":[],"next_page_id":"e9"}`))
			},
			call: func(h *Harness) error {
				evs, cur, err := h.Next(t.Context(), Cursor{})
				if err == nil && (len(evs) != 0 || cur != (Cursor{})) {
					return errors.New("the cursor moved over nothing")
				}
				_, err = h.Skip(t.Context(), 3)
				return err
			},
			want: func(err error) bool { return err == nil },
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(c.handler)
			t.Cleanup(srv.Close)
			if err := c.call(NewHarness(srv.URL, conv)); !c.want(err) {
				t.Fatalf("got %v", err)
			}
		})
	}
}
