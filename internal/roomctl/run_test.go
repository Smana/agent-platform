// SPDX-License-Identifier: Apache-2.0

package roomctl

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/runwatch"
	"github.com/Smana/agent-platform/internal/wire"
)

const testClaim = `{"apiVersion":"agents.ogenki.io/v1alpha1","kind":"AgentRun","metadata":{"name":"run-a2b3c4d5","namespace":"agents"},"spec":{"role":"implementer"}}`

// runBroker is a local broker for roomctl run: plain http, no token, one room, one run.
type runBroker struct {
	createStatus int               // 0: 201
	reject       map[string]string // act kind → rejection
	claim        string            // start_run's ack result; "" sends none
	end          string            // the run's terminal phase
	notFound     int               // WebSocket dials answered 404 first (the room's log row is not there yet); <0: always
	dials        int
	stray        string // another run's id, whose Failed phase arrives first
	mu           sync.Mutex
	acts         []map[string]any
	created      int
}

func (b *runBroker) start(t *testing.T) Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/rooms" {
			b.mu.Lock()
			b.created++
			b.mu.Unlock()
			if b.createStatus != 0 {
				http.Error(w, "no such repository", b.createStatus)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"r2om2abc"}`))
			return
		}
		b.mu.Lock()
		b.dials++
		missing := b.notFound < 0 || b.dials <= b.notFound
		b.mu.Unlock()
		if missing {
			http.NotFound(w, r)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.CloseNow() }()
		var hello wire.ClientFrame
		if wsjson.Read(r.Context(), c, &hello) != nil {
			return
		}
		if hello.AfterSeq == nil || *hello.AfterSeq != math.MaxInt64 { // a follow, not an act
			_ = wsjson.Write(r.Context(), c, wire.ServerFrame{Type: wire.FrameState, Snapshot: &wire.Snapshot{RoomID: hello.RoomID}})
			if b.stray != "" {
				ev := envelope.Event{Seq: 9, RoomID: hello.RoomID, RunID: b.stray, Type: envelope.StateChanged,
					Actor:   envelope.Actor{Kind: envelope.ActorSystem, ID: "system:room-broker"},
					Payload: envelope.StatePayload("run_phase", map[string]any{"phase": "Failed", "reason": "other"})}
				_ = wsjson.Write(r.Context(), c, wire.ServerFrame{Type: wire.FrameEvent, Event: &ev})
			}
			for i, phase := range []string{"Running", b.end} {
				ev := envelope.Event{Seq: int64(10 + i), RoomID: hello.RoomID, RunID: "a2b3c4d5", Type: envelope.StateChanged,
					Actor:   envelope.Actor{Kind: envelope.ActorSystem, ID: "system:room-broker"},
					Payload: envelope.StatePayload("run_phase", map[string]any{"phase": phase, "reason": "agent_finished"})}
				_ = wsjson.Write(r.Context(), c, wire.ServerFrame{Type: wire.FrameEvent, Event: &ev})
			}
			var f wire.ClientFrame
			_ = wsjson.Read(r.Context(), c, &f) // until roomctl closes
			return
		}
		for {
			var f wire.ClientFrame
			if wsjson.Read(r.Context(), c, &f) != nil {
				return
			}
			var act map[string]any
			_ = json.Unmarshal(f.Action, &act)
			b.mu.Lock()
			b.acts = append(b.acts, act)
			b.mu.Unlock()
			ack := wire.ServerFrame{Type: wire.FrameAck, ClientSeq: f.ClientSeq, Seq: 3}
			kind, _ := act["kind"].(string)
			if why := b.reject[kind]; why != "" {
				ack.Rejected, ack.Seq = why, 0
			} else if kind == "start_run" && b.claim != "" {
				ack.Result = json.RawMessage(b.claim)
			}
			_ = wsjson.Write(r.Context(), c, ack)
		}
	}))
	t.Cleanup(srv.Close)
	return Client{URL: srv.URL, HC: srv.Client(), Stream: srv.Client(), Token: func(context.Context) (string, error) { return "", nil }}
}

func TestQuickstartRunHappyPath(t *testing.T) {
	b := &runBroker{claim: testClaim, end: "Succeeded"}
	var applied []byte
	var out strings.Builder
	end, err := QuickstartRun{Client: b.start(t), Repository: "owner/repo", Task: "Fix a typo in the README", Role: "implementer",
		Apply: func(_ context.Context, m []byte) error { applied = m; return nil }, Out: &out}.Do(t.Context())
	if err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if end != (RunEnd{Room: "r2om2abc", Claim: "agents/run-a2b3c4d5", Phase: "Succeeded", Reason: "agent_finished"}) {
		t.Fatalf("end %+v", end)
	}
	if string(applied) != testClaim {
		t.Fatalf("applied %s", applied)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	want := []map[string]any{{"kind": "message", "delivery": "queued", "text": "Fix a typo in the README"}, {"kind": "start_run", "role": "implementer"}}
	if len(b.acts) != 2 || !mapsEqual(b.acts[0], want[0]) || !mapsEqual(b.acts[1], want[1]) {
		t.Fatalf("acts %v", b.acts)
	}
	for _, line := range []string{"room r2om2abc: ", "/r/r2om2abc\n", "task queued: seq 3\n", "run requested: agents/run-a2b3c4d5\n",
		"run agents/run-a2b3c4d5 ended Succeeded: agent_finished\n"} {
		if !strings.Contains(out.String(), line) {
			t.Errorf("output lacks %q:\n%s", line, out.String())
		}
	}
}

func mapsEqual(a, b map[string]any) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}

func TestQuickstartRunStopsAtTheFailingStep(t *testing.T) {
	for _, tc := range []struct {
		name     string
		broker   *runBroker
		apply    error
		step     string
		contains string
	}{
		{"room", &runBroker{createStatus: http.StatusNotFound}, nil, "room", "404: no such repository"},
		{"task", &runBroker{reject: map[string]string{"message": "sealed"}, claim: testClaim}, nil, "task", "sealed"},
		{"busy", &runBroker{reject: map[string]string{"start_run": "room_busy"}, claim: testClaim}, nil, "start_run", "room_busy: a run is already running"},
		{"not permitted", &runBroker{reject: map[string]string{"start_run": "not_permitted"}, claim: testClaim}, nil, "start_run", "not_permitted"},
		{"no claim", &runBroker{}, nil, "start_run", "no claim to apply"},
		{"kubectl", &runBroker{claim: testClaim, end: "Succeeded"}, errors.New(`error: the server doesn't have a resource type "agentruns"`), "claim", `resource type "agentruns"`},
		{"failed run", &runBroker{claim: testClaim, end: "Failed"}, nil, "run", "ended Failed: agent_finished"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := QuickstartRun{Client: tc.broker.start(t), Repository: "owner/repo", Task: "x", Role: "implementer",
				Apply: func(context.Context, []byte) error { return tc.apply }, Out: io.Discard}.Do(t.Context())
			var se *StepError
			if !errors.As(err, &se) || se.Step != tc.step || !strings.Contains(err.Error(), tc.contains) {
				t.Fatalf("err %v, want step %s containing %q", err, tc.step, tc.contains)
			}
		})
	}
}

// A run id that is only a tail of the claim's name is another run: its end is not this run's.
func TestQuickstartRunMatchesItsRunExactly(t *testing.T) {
	b := &runBroker{claim: testClaim, end: "Succeeded", stray: "b3c4d5"}
	end, err := QuickstartRun{Client: b.start(t), Repository: "owner/repo", Task: "x", Role: "implementer",
		Apply: func(context.Context, []byte) error { return nil }, Out: io.Discard}.Do(t.Context())
	if err != nil || end.Phase != "Succeeded" {
		t.Fatalf("end %+v, err %v", end, err)
	}
}

// POST /api/rooms returns before the room's log row exists: the first act can 404, and nothing
// has been written yet, so it is retried until the room is there.
func TestQuickstartRunWaitsForTheRoomRow(t *testing.T) {
	b := &runBroker{claim: testClaim, end: "Succeeded", notFound: 3}
	_, err := QuickstartRun{Client: b.start(t), Repository: "owner/repo", Task: "x", Role: "implementer",
		Apply: func(context.Context, []byte) error { return nil }, Out: io.Discard,
		SettleStep: time.Millisecond, SettleFor: 5 * time.Second}.Do(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.acts) != 2 || b.acts[0]["kind"] != "message" {
		t.Fatalf("acts %v: the task must be sent exactly once", b.acts)
	}
}

func TestQuickstartRunGivesUpOnARoomThatNeverAppears(t *testing.T) {
	b := &runBroker{claim: testClaim, end: "Succeeded", notFound: -1}
	_, err := QuickstartRun{Client: b.start(t), Repository: "owner/repo", Task: "x", Role: "implementer",
		Apply: func(context.Context, []byte) error { t.Fatal("applied"); return nil }, Out: io.Discard,
		SettleStep: time.Millisecond, SettleFor: 50 * time.Millisecond}.Do(t.Context())
	var se *StepError
	var ref *Refusal
	if !errors.As(err, &se) || se.Step != "task" || !errors.As(err, &ref) || ref.Code != http.StatusNotFound {
		t.Fatalf("err %v, want a task step wrapping a 404", err)
	}
}

func TestQuickstartRunRefusesRolesBeforeAnyRequest(t *testing.T) {
	b := &runBroker{claim: testClaim, end: "Succeeded"}
	_, err := QuickstartRun{Client: b.start(t), Repository: "owner/repo", Task: "x", Role: "reviewer",
		Apply: func(context.Context, []byte) error { return nil }, Out: io.Discard}.Do(t.Context())
	b.mu.Lock()
	defer b.mu.Unlock()
	if err == nil || !strings.Contains(err.Error(), "implementer, a tester or a triager") || b.created != 0 {
		t.Fatalf("err %v, rooms created %d", err, b.created)
	}
}

// roomctl cannot import runwatch (it would pull client-go into the CLI): its copy of the
// terminal phases must match.
func TestTerminalPhasesMatchRunwatch(t *testing.T) {
	for _, p := range []string{"Pending", "Running", "Succeeded", "Failed", "BudgetExhausted", "Revoked", "Suspended"} {
		if terminalPhases[p] != runwatch.Terminal(p) {
			t.Errorf("%s: roomctl %v, runwatch %v", p, terminalPhases[p], runwatch.Terminal(p))
		}
	}
}
