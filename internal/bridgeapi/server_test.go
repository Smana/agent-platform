// SPDX-License-Identifier: Apache-2.0

package bridgeapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/redact"
	"github.com/Smana/agent-platform/internal/runwatch"
	"github.com/Smana/agent-platform/internal/store"
	"github.com/Smana/agent-platform/internal/wire"
)

const (
	room    = "3kq7x2ma"
	runA    = "7f3cq2xz"
	runB    = "aaaaaaaa"
	factory = "system:factory"
)

// The landed authenticators satisfy the handlers' interface.
var (
	_ Authenticator = (*authn.Runs)(nil)
	_ Authenticator = (*authn.Systems)(nil)
)

// tokenAuth maps "Bearer run:<id>" / "Bearer sys:<principal>" to principals, so these
// tests exercise the handlers; authn has its own tests. "sys:forbidden" is a valid
// token outside the allowlist.
type tokenAuth struct{ prefix string }

func (a tokenAuth) Authenticate(r *http.Request) (authn.Principal, error) {
	v, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "+a.prefix)
	if !ok {
		return authn.Principal{}, authn.ErrUnauthenticated
	}
	if a.prefix == "run:" {
		return authn.Principal{Kind: envelope.ActorAgent, ID: "agent:" + v, RunID: v, Expiry: time.Now().Add(time.Hour)}, nil
	}
	if v == "forbidden" {
		return authn.Principal{}, authn.ErrForbidden
	}
	return authn.Principal{Kind: envelope.ActorSystem, ID: v}, nil
}

// memLog is the log in memory, with the store's idempotency, fencing and
// sealing semantics. refuse fails chosen appends; touchHeld makes TouchBridge
// report the lease held whoever holds it, as a lease lost between the touch and
// the append does.
type memLog struct {
	mu        sync.Mutex
	events    map[string][]envelope.Event
	keys      map[string]int64
	cursors   map[string]int64  // room/originClient -> highest originSeq
	leases    map[string]string // room -> the run holding its bridge lease (no expiry here)
	refuse    func(envelope.Draft) error
	touchHeld bool
	touches   int // lease renewals
}

func newMemLog() *memLog {
	return &memLog{events: map[string][]envelope.Event{room: nil, "abcdefgh": nil}, keys: map[string]int64{},
		cursors: map[string]int64{}, leases: map[string]string{}}
}

func (m *memLog) Append(_ context.Context, d envelope.Draft) (envelope.Event, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.appendLocked(d)
}

func (m *memLog) appendLocked(d envelope.Draft) (envelope.Event, bool, error) {
	if err := d.Validate(); err != nil {
		return envelope.Event{}, false, err
	}
	if _, ok := m.events[d.RoomID]; !ok {
		return envelope.Event{}, false, store.ErrNoRoom
	}
	k := fmt.Sprintf("%s/%s/%d", d.RoomID, d.OriginClient, d.OriginSeq)
	if seq, ok := m.keys[k]; ok {
		return m.events[d.RoomID][seq-1], true, nil
	}
	if m.refuse != nil {
		if err := m.refuse(d); err != nil {
			return envelope.Event{}, false, err
		}
	}
	ev := envelope.Event{Seq: int64(len(m.events[d.RoomID]) + 1), RoomID: d.RoomID, RunID: d.RunID,
		Actor: d.Actor, Type: d.Type, Origin: d.Origin, Payload: d.Payload, Redactions: d.Redactions}
	m.events[d.RoomID] = append(m.events[d.RoomID], ev)
	m.keys[k] = ev.Seq
	if c := d.RoomID + "/" + d.OriginClient; d.OriginSeq > m.cursors[c] {
		m.cursors[c] = d.OriginSeq
	}
	return ev, false, nil
}

func (m *memLog) AppendAsBridge(_ context.Context, bridgeRun string, d envelope.Draft) (envelope.Event, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.leases[d.RoomID] != bridgeRun {
		return envelope.Event{}, false, store.ErrLeaseLost
	}
	return m.appendLocked(d)
}

func (m *memLog) Range(_ context.Context, roomID string, after int64, limit int) ([]envelope.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []envelope.Event
	for _, e := range m.events[roomID] {
		if e.Seq > after && len(out) < limit {
			out = append(out, e)
		}
	}
	return out, nil
}

func (m *memLog) Cursor(_ context.Context, roomID, client string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cursors[roomID+"/"+client], nil
}

func (m *memLog) Room(_ context.Context, id string) (store.RoomState, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	evs, ok := m.events[id]
	if !ok {
		return store.RoomState{ID: id}, store.ErrNoRoom
	}
	return store.RoomState{ID: id, LastSeq: int64(len(evs))}, nil
}

func (m *memLog) ClaimBridge(ctx context.Context, roomID, runID string, _ time.Duration, live func(context.Context, string) bool) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.events[roomID]; !ok {
		return "", false, store.ErrNoRoom
	}
	if h, ok := m.leases[roomID]; ok && h != runID && live(ctx, h) {
		return h, false, nil
	}
	m.leases[roomID] = runID
	return runID, true, nil
}

func (m *memLog) TouchBridge(_ context.Context, roomID, runID string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.touches++
	return m.touchHeld || m.leases[roomID] == runID, nil
}

func (m *memLog) stored() []envelope.Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]envelope.Event(nil), m.events[room]...)
}

func agentRun(id, roomID, phase string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"metadata": map[string]any{"name": "xplane-run-" + id, "namespace": "agents"},
		"spec":     map[string]any{"roomRef": roomID, "role": "implementer"},
		"status":   map[string]any{"phase": phase},
	}}
}

// manualTicker is the stream's keep-alive clock, advanced by the test.
type manualTicker chan time.Time

func (c manualTicker) new(time.Duration) (<-chan time.Time, func()) { return c, func() {} }

func newServer(t *testing.T) (*Server, *memLog, *runwatch.Watcher) {
	t.Helper()
	red, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}
	log := newMemLog()
	w := runwatch.New()
	s := &Server{Log: log, Redactor: red, Runs: tokenAuth{"run:"}, Systems: tokenAuth{"sys:"}, Watch: w,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	w.OnGone(s.Drop)
	return s, log, w
}

func call(t *testing.T, h http.Handler, method, path, token string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var b bytes.Buffer
	switch v := body.(type) {
	case nil:
	case []byte:
		b.Write(v)
	default:
		if err := json.NewEncoder(&b).Encode(v); err != nil {
			t.Fatal(err)
		}
	}
	r := httptest.NewRequestWithContext(t.Context(), method, path, &b)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

func reason(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()
	var e wire.Error
	_ = json.Unmarshal(rec.Body.Bytes(), &e)
	return e.Reason
}

func chat(seq int64, text string) wire.Item {
	return wire.Item{Stream: wire.StreamEvents, Seq: seq, Type: envelope.Message,
		Payload: envelope.Must(envelope.MessagePayload{Kind: envelope.KindChat, Text: text, Delivery: envelope.DeliveryNone})}
}

func batch(items ...wire.Item) wire.Batch { return wire.Batch{Items: items} }

// hello admits run on room's lease, as its bridge does before pushing.
func hello(t *testing.T, h http.Handler, run string) {
	t.Helper()
	if rec := call(t, h, http.MethodPost, "/v1/bridge/hello", "run:"+run, nil); rec.Code != http.StatusOK {
		t.Fatalf("hello %s: %d %s", run, rec.Code, rec.Body)
	}
}

func TestIngestStampsActorAndResumes(t *testing.T) {
	s, log, w := newServer(t)
	w.Upsert(t.Context(), agentRun(runA, room, "Running"))
	h := s.Routes()
	hello(t, h, runA)
	status := wire.Item{Stream: wire.StreamStatus, Seq: 3, Type: envelope.StateChanged,
		Payload: envelope.StatePayload("harness_status", map[string]any{"status": "running"})}
	rec := call(t, h, http.MethodPost, "/v1/bridge/events", "run:"+runA, batch(chat(4, "one"), status, chat(8, "two")))
	if rec.Code != http.StatusOK {
		t.Fatalf("ingest: %d %s", rec.Code, rec.Body)
	}
	var ack wire.BatchAck
	if err := json.Unmarshal(rec.Body.Bytes(), &ack); err != nil || ack != (wire.BatchAck{AfterHarnessSeq: 8, AfterStatusSeq: 3}) {
		t.Fatalf("ack = %+v (%v)", ack, err)
	}
	if log.touches != 1 {
		t.Fatalf("the batch renewed the lease %d times, want 1", log.touches)
	}
	// The same batch again (a retry after a lost response) appends nothing.
	call(t, h, http.MethodPost, "/v1/bridge/events", "run:"+runA, batch(chat(4, "one"), status, chat(8, "two")))
	evs := log.stored()
	if len(evs) != 3 {
		t.Fatalf("%d events, want 3", len(evs))
	}
	for _, ev := range evs {
		if ev.Actor != (envelope.Actor{Kind: envelope.ActorAgent, ID: "agent:" + runA, Role: "implementer"}) ||
			ev.Origin != envelope.OriginHarness || ev.RunID != runA {
			t.Fatalf("event = %+v", ev)
		}
	}
	rec = call(t, h, http.MethodPost, "/v1/bridge/hello", "run:"+runA, nil)
	var res wire.Resume
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if res.RoomID != room || res.AfterHarnessSeq != 8 || res.AfterStatusSeq != 3 {
		t.Fatalf("resume = %+v", res)
	}
}

func TestSecretsNeverReachTheLog(t *testing.T) {
	secret := "ghs_" + "Zq8mR2tXv9LkPw4NcYb7HsJ1fGdE6aUo3iTe" // pragma: allowlist secret (36 chars after ghs_)
	cases := []struct {
		name string
		send func(t *testing.T, h http.Handler) int
	}{
		{"a bridge's chat", func(t *testing.T, h http.Handler) int {
			return call(t, h, http.MethodPost, "/v1/bridge/events", "run:"+runA, batch(chat(4, "token "+secret))).Code
		}},
		{"a system caller's task_state", func(t *testing.T, h http.Handler) int {
			return call(t, h, http.MethodPost, "/v1/rooms/"+room+"/messages", "sys:"+factory,
				map[string]any{"kind": "task_state", "text": "token " + secret, "clientSeq": 1}).Code
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, log, w := newServer(t)
			w.Upsert(t.Context(), agentRun(runA, room, "Running"))
			h := s.Routes()
			hello(t, h, runA)
			if code := c.send(t, h); code >= 300 {
				t.Fatalf("refused: %d", code)
			}
			evs := log.stored()
			if len(evs) != 1 {
				t.Fatalf("%d events", len(evs))
			}
			if strings.Contains(string(evs[0].Payload), secret) || len(evs[0].Redactions) == 0 {
				t.Fatalf("payload %s, redactions %v", evs[0].Payload, evs[0].Redactions)
			}
		})
	}
}

func TestOnlyLiveRunsWithARoomAreAdmitted(t *testing.T) {
	cases := []struct {
		name   string
		claim  *unstructured.Unstructured
		token  string
		code   int
		reason string
	}{
		{"no token", nil, "", http.StatusUnauthorized, wire.ReasonUnauthenticated},
		{"a system token", agentRun(runA, room, "Running"), "sys:" + factory, http.StatusUnauthorized, wire.ReasonUnauthenticated},
		{"an unknown run", nil, "run:" + runA, http.StatusForbidden, wire.ReasonRunNotLive},
		{"a terminal run", agentRun(runA, room, "Succeeded"), "run:" + runA, http.StatusForbidden, wire.ReasonRunNotLive},
		{"a run with no room", agentRun(runA, "", "Running"), "run:" + runA, http.StatusForbidden, wire.ReasonRunHasNoRoom},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, log, w := newServer(t)
			if c.claim != nil {
				w.Upsert(t.Context(), c.claim)
			}
			h := s.Routes()
			for _, r := range []struct{ method, path string }{
				{http.MethodPost, "/v1/bridge/hello"}, {http.MethodPost, "/v1/bridge/events"}, {http.MethodGet, "/v1/bridge/stream"},
			} {
				rec := call(t, h, r.method, r.path, c.token, batch(chat(1, "x")))
				if rec.Code != c.code || reason(t, rec) != c.reason {
					t.Errorf("%s %s: %d %s, want %d %s", r.method, r.path, rec.Code, reason(t, rec), c.code, c.reason)
				}
			}
			if n := len(log.stored()); n != 0 {
				t.Fatalf("%d events written", n)
			}
		})
	}
}

// Ruling P17: one live run per room.
func TestSecondRunInARoomIsBusy(t *testing.T) {
	s, log, w := newServer(t)
	h := s.Routes()
	w.Upsert(t.Context(), agentRun(runA, room, "Running"))
	w.Upsert(t.Context(), agentRun(runB, room, "Running"))
	hello(t, h, runA)
	rec := call(t, h, http.MethodPost, "/v1/bridge/hello", "run:"+runB, nil)
	if rec.Code != http.StatusConflict || reason(t, rec) != wire.ReasonRoomBusy {
		t.Fatalf("second run: %d %s", rec.Code, rec.Body)
	}
	evs := log.stored()
	last := evs[len(evs)-1]
	if !strings.Contains(string(last.Payload), "concurrent_run") || last.Origin != envelope.OriginBroker {
		t.Fatalf("no limit event: %+v", last)
	}
	// Once the holder ends, the room is free.
	w.Upsert(t.Context(), agentRun(runA, room, "Succeeded"))
	hello(t, h, runB)
}

// Review I7: two replicas share the lease through the log, not through memory.
func TestTheLeaseHoldsAcrossReplicas(t *testing.T) {
	a, log, w := newServer(t)
	b := &Server{Log: log, Redactor: a.Redactor, Runs: a.Runs, Systems: a.Systems, Watch: w, Logger: a.Logger}
	w.Upsert(t.Context(), agentRun(runA, room, "Running"))
	w.Upsert(t.Context(), agentRun(runB, room, "Running"))
	hello(t, a.Routes(), runA)
	if rec := call(t, b.Routes(), http.MethodPost, "/v1/bridge/hello", "run:"+runB, nil); rec.Code != http.StatusConflict {
		t.Fatalf("second run on the other replica: %d", rec.Code)
	}
}

// Ruling Y: a displaced bridge learns it from a 409 and never appends again.
func TestADisplacedBridgeGets409AndWritesNothing(t *testing.T) {
	cases := []struct {
		name      string
		touchHeld bool // the lease moves between TouchBridge and the append
	}{
		{"the touch sees another holder", false},
		{"the append's fence sees another holder", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, log, w := newServer(t)
			h := s.Routes()
			w.Upsert(t.Context(), agentRun(runA, room, "Running"))
			hello(t, h, runA)
			log.mu.Lock()
			log.leases[room], log.touchHeld = runB, c.touchHeld
			log.mu.Unlock()
			rec := call(t, h, http.MethodPost, "/v1/bridge/events", "run:"+runA, batch(chat(4, "late")))
			if rec.Code != http.StatusConflict || reason(t, rec) != wire.ReasonLeaseLost {
				t.Fatalf("displaced bridge: %d %s", rec.Code, rec.Body)
			}
			if n := len(log.stored()); n != 0 {
				t.Fatalf("%d events written by a displaced bridge", n)
			}
		})
	}
}

func TestEventsWithoutHelloLoseTheLease(t *testing.T) {
	s, log, w := newServer(t)
	w.Upsert(t.Context(), agentRun(runA, room, "Running"))
	rec := call(t, s.Routes(), http.MethodPost, "/v1/bridge/events", "run:"+runA, batch(chat(4, "x")))
	if rec.Code != http.StatusConflict || len(log.stored()) != 0 {
		t.Fatalf("no hello: %d, %d events", rec.Code, len(log.stored()))
	}
}

// Review M5: a bridge pushes what its mapping produces, and cannot forge the rest.
// A refused item refuses the whole batch before anything is written.
func TestABridgeCannotForgeVerdictsOrDecisions(t *testing.T) {
	raw := func(s string) json.RawMessage { return json.RawMessage(s) }
	cases := []struct {
		name   string
		item   wire.Item
		reason string
	}{
		{"a review verdict", wire.Item{Stream: wire.StreamEvents, Seq: 4, Type: envelope.Message, Payload: envelope.Must(envelope.MessagePayload{
			Kind: envelope.KindReviewVerdict, Verdict: "approve", Delivery: envelope.DeliveryNone})}, wire.ReasonBadItem},
		{"a task_state", wire.Item{Stream: wire.StreamEvents, Seq: 4, Type: envelope.Message, Payload: envelope.Must(envelope.MessagePayload{
			Kind: envelope.KindTaskState, Delivery: envelope.DeliveryNone})}, wire.ReasonBadItem},
		{"a queued chat", wire.Item{Stream: wire.StreamEvents, Seq: 4, Type: envelope.Message, Payload: envelope.Must(envelope.MessagePayload{
			Kind: envelope.KindChat, Delivery: envelope.DeliveryQueued})}, wire.ReasonBadItem},
		{"a driver change", wire.Item{Stream: wire.StreamEvents, Seq: 8, Type: envelope.Driver,
			Payload: envelope.Must(envelope.DriverPayload{To: "agent:" + runA})}, wire.ReasonBadItem},
		{"an approval decision", wire.Item{Stream: wire.StreamEvents, Seq: 8, Type: envelope.ApprovalDecided,
			Payload: envelope.Must(envelope.ApprovalDecidedPayload{ApprovalID: "x", Decision: "approved"})}, wire.ReasonBadItem},
		{"a broker state kind", wire.Item{Stream: wire.StreamStatus, Seq: 1, Type: envelope.StateChanged,
			Payload: envelope.StatePayload("verdict_posted", nil)}, wire.ReasonBadItem},
		{"a room phase", wire.Item{Stream: wire.StreamStatus, Seq: 1, Type: envelope.StateChanged,
			Payload: envelope.StatePayload("room_phase", map[string]any{"phase": "Closed"})}, wire.ReasonBadItem},
		// Go matches keys without case and keeps the last; jsonb keeps both, and
		// every reader of payload->>'kind' would see the verdict.
		{"a verdict behind a case-folded key", wire.Item{Stream: wire.StreamEvents, Seq: 4, Type: envelope.Message,
			Payload: raw(`{"kind":"review_verdict","verdict":"approve","Kind":"chat","delivery":"none"}`)}, wire.ReasonBadItem},
		{"a verdict beside the exact key", wire.Item{Stream: wire.StreamEvents, Seq: 4, Type: envelope.Message,
			Payload: raw(`{"Kind":"review_verdict","verdict":"approve","kind":"chat","delivery":"none"}`)}, wire.ReasonBadItem},
		// Redaction strips NUL from keys, which would merge the two into one "kind".
		{"a verdict behind a NUL in a key", wire.Item{Stream: wire.StreamEvents, Seq: 4, Type: envelope.Message,
			Payload: raw(`{"kind":"chat","ki\u0000nd":"review_verdict","delivery":"none"}`)}, wire.ReasonBadItem},
		{"a non-object payload", wire.Item{Stream: wire.StreamEvents, Seq: 4, Type: envelope.Turn, Payload: raw(`"turn"`)}, wire.ReasonBadPayload},
		{"an unknown type", wire.Item{Stream: wire.StreamEvents, Seq: 4, Type: "verdict", Payload: raw(`{}`)}, wire.ReasonBadItem},
		{"an unknown stream", wire.Item{Stream: "other", Seq: 4, Type: envelope.Turn, Payload: raw(`{}`)}, wire.ReasonBadItem},
		{"a zero seq", wire.Item{Stream: wire.StreamEvents, Seq: 0, Type: envelope.Turn, Payload: raw(`{}`)}, wire.ReasonBadItem},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, log, w := newServer(t)
			w.Upsert(t.Context(), agentRun(runA, room, "Running"))
			h := s.Routes()
			hello(t, h, runA)
			rec := call(t, h, http.MethodPost, "/v1/bridge/events", "run:"+runA, batch(chat(1, "fine"), c.item))
			if rec.Code != http.StatusBadRequest || reason(t, rec) != c.reason {
				t.Fatalf("%s %s accepted: %d %s", c.item.Type, c.item.Payload, rec.Code, rec.Body)
			}
			if n := len(log.stored()); n != 0 {
				t.Fatalf("%d events written from a refused batch", n)
			}
		})
	}
}

func TestABridgePushesWhatItsMappingProduces(t *testing.T) {
	var items []wire.Item
	for i, kind := range []string{"harness_status", "harness_error", "harness_paused", "harness_event",
		"delivered", "interrupted", "policy_decision", "decision_applied"} {
		items = append(items, wire.Item{Stream: wire.StreamStatus, Seq: int64(i + 1), Type: envelope.StateChanged,
			Payload: envelope.StatePayload(kind, nil)})
	}
	items = append(items,
		chat(1, "hi"),
		wire.Item{Stream: wire.StreamEvents, Seq: 2, Type: envelope.Message, Payload: json.RawMessage(`{"kind":"chat","text":"x"}`)},
		wire.Item{Stream: wire.StreamEvents, Seq: 3, Type: envelope.Turn, Payload: envelope.Must(envelope.TurnPayload{RunID: runA, TurnID: "t", Phase: "started"})},
		wire.Item{Stream: wire.StreamEvents, Seq: 4, Type: envelope.ToolCall, Payload: envelope.Must(envelope.ToolCallPayload{CallID: "c", Tool: "bash"})},
		wire.Item{Stream: wire.StreamEvents, Seq: 5, Type: envelope.ToolResult, Payload: envelope.Must(envelope.ToolResultPayload{CallID: "c", Status: "ok"})},
	)
	s, log, w := newServer(t)
	w.Upsert(t.Context(), agentRun(runA, room, "Running"))
	h := s.Routes()
	hello(t, h, runA)
	if rec := call(t, h, http.MethodPost, "/v1/bridge/events", "run:"+runA, batch(items...)); rec.Code != http.StatusOK {
		t.Fatalf("refused: %d %s", rec.Code, rec.Body)
	}
	if n := len(log.stored()); n != len(items) {
		t.Fatalf("%d events, want %d", n, len(items))
	}
}

func TestBatchBounds(t *testing.T) {
	big := make([]wire.Item, maxBatchItems+1)
	for i := range big {
		big[i] = chat(int64(i+1), "x")
	}
	cases := []struct {
		name   string
		body   any
		code   int
		reason string
	}{
		{"over the byte cap", batch(chat(1, strings.Repeat("x", maxBatchBytes))), http.StatusRequestEntityTooLarge, wire.ReasonBatchTooLarge},
		{"over the item cap", batch(big...), http.StatusRequestEntityTooLarge, wire.ReasonBatchTooLarge},
		{"an unknown field", []byte(`{"items":[],"actor":{"kind":"system"}}`), http.StatusBadRequest, wire.ReasonBadBatch},
		{"trailing data", []byte(`{"items":[]} {"items":[]}`), http.StatusBadRequest, wire.ReasonBadBatch},
		{"not JSON", []byte(`items`), http.StatusBadRequest, wire.ReasonBadBatch},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, log, w := newServer(t)
			w.Upsert(t.Context(), agentRun(runA, room, "Running"))
			h := s.Routes()
			hello(t, h, runA)
			rec := call(t, h, http.MethodPost, "/v1/bridge/events", "run:"+runA, c.body)
			if rec.Code != c.code || reason(t, rec) != c.reason {
				t.Fatalf("%d %s, want %d %s", rec.Code, rec.Body, c.code, c.reason)
			}
			if n := len(log.stored()); n != 0 {
				t.Fatalf("%d events written", n)
			}
		})
	}
}

// Review I6: a payload the database refuses keeps its slot as a stub, so the cursor moves on.
func TestARefusedPayloadBecomesAStub(t *testing.T) {
	s, log, w := newServer(t)
	w.Upsert(t.Context(), agentRun(runA, room, "Running"))
	h := s.Routes()
	hello(t, h, runA)
	log.refuse = func(d envelope.Draft) error {
		if strings.Contains(string(d.Payload), "poison") {
			return &pgconn.PgError{Code: "22P05", Detail: "poison"}
		}
		return nil
	}
	if rec := call(t, h, http.MethodPost, "/v1/bridge/events", "run:"+runA, batch(chat(4, "poison"))); rec.Code != http.StatusOK {
		t.Fatalf("refused payload: %d %s", rec.Code, rec.Body)
	}
	if ev := log.stored()[0]; !strings.Contains(string(ev.Payload), `"refused":true`) || ev.Type != envelope.Message {
		t.Fatalf("stub = %s %s", ev.Type, ev.Payload)
	}
}

func TestLogFailuresMapToStatus(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		code   int
		reason string
		system bool // the system API meets it too; only a bridge holds a lease
	}{
		{"a sealed room", fmt.Errorf("store: append: %w", store.ErrSealed), http.StatusGone, wire.ReasonSealed, true},
		{"a lost lease", fmt.Errorf("store: append: %w", store.ErrLeaseLost), http.StatusConflict, wire.ReasonLeaseLost, false},
		{"a database outage", errors.New("connection refused"), http.StatusServiceUnavailable, wire.ReasonLogUnavailable, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, log, w := newServer(t)
			w.Upsert(t.Context(), agentRun(runA, room, "Running"))
			h := s.Routes()
			hello(t, h, runA)
			log.refuse = func(envelope.Draft) error { return c.err }
			rec := call(t, h, http.MethodPost, "/v1/bridge/events", "run:"+runA, batch(chat(4, "x")))
			if rec.Code != c.code || reason(t, rec) != c.reason {
				t.Errorf("bridge: %d %s, want %d %s", rec.Code, rec.Body, c.code, c.reason)
			}
			rec = call(t, h, http.MethodPost, "/v1/rooms/"+room+"/messages", "sys:"+factory,
				map[string]any{"kind": "task_state", "text": "x", "clientSeq": 1})
			if c.system && (rec.Code != c.code || reason(t, rec) != c.reason) {
				t.Errorf("system: %d %s, want %d %s", rec.Code, rec.Body, c.code, c.reason)
			}
		})
	}
}

func TestSystemAPI(t *testing.T) {
	s, _, w := newServer(t)
	w.Upsert(t.Context(), agentRun(runA, room, "Running"))
	h := s.Routes()
	hello(t, h, runA)
	call(t, h, http.MethodPost, "/v1/bridge/events", "run:"+runA, batch(chat(4, "one")))

	msg := func(kind, text string, seq int64) map[string]any {
		return map[string]any{"kind": kind, "text": text, "clientSeq": seq}
	}
	writes := []struct {
		name   string
		path   string
		token  string
		body   any
		code   int
		reason string
	}{
		{"a task_state", "/v1/rooms/" + room + "/messages", "sys:" + factory, msg("task_state", "Reviewing", 1), http.StatusCreated, ""},
		{"the same task_state again", "/v1/rooms/" + room + "/messages", "sys:" + factory, msg("task_state", "Reviewing", 1), http.StatusOK, ""},
		{"a chat", "/v1/rooms/" + room + "/messages", "sys:" + factory, msg("chat", "hi", 2), http.StatusBadRequest, wire.ReasonBadMessage},
		{"no clientSeq", "/v1/rooms/" + room + "/messages", "sys:" + factory, msg("task_state", "x", 0), http.StatusBadRequest, wire.ReasonBadMessage},
		{"an overlong text", "/v1/rooms/" + room + "/messages", "sys:" + factory, msg("task_state", strings.Repeat("x", envelope.MaxHumanMessage+1), 3), http.StatusBadRequest, wire.ReasonBadMessage},
		{"an unknown field", "/v1/rooms/" + room + "/messages", "sys:" + factory, map[string]any{"kind": "task_state", "text": "x", "clientSeq": 4, "actor": "human:x"}, http.StatusBadRequest, wire.ReasonBadMessage},
		{"a bad room id", "/v1/rooms/NOPE/messages", "sys:" + factory, msg("task_state", "x", 5), http.StatusBadRequest, wire.ReasonBadRoom},
		{"a room with no log", "/v1/rooms/zzzzzzzz/messages", "sys:" + factory, msg("task_state", "x", 6), http.StatusNotFound, wire.ReasonNoRoom},
		{"a run token", "/v1/rooms/" + room + "/messages", "run:" + runA, msg("task_state", "x", 7), http.StatusUnauthorized, wire.ReasonUnauthenticated},
		{"an unlisted system caller", "/v1/rooms/" + room + "/messages", "sys:forbidden", msg("task_state", "x", 8), http.StatusForbidden, wire.ReasonNotPermitted},
	}
	for _, c := range writes {
		t.Run(c.name, func(t *testing.T) {
			rec := call(t, h, http.MethodPost, c.path, c.token, c.body)
			if rec.Code != c.code || reason(t, rec) != c.reason {
				t.Fatalf("%d %s, want %d %q", rec.Code, rec.Body, c.code, c.reason)
			}
		})
	}

	reads := []struct {
		name   string
		path   string
		token  string
		code   int
		events int
	}{
		{"the whole room", "/v1/rooms/" + room + "/events?afterSeq=0&limit=10", "sys:" + factory, http.StatusOK, 2},
		{"after a seq", "/v1/rooms/" + room + "/events?afterSeq=1", "sys:" + factory, http.StatusOK, 1},
		{"one at a time", "/v1/rooms/" + room + "/events?limit=1", "sys:" + factory, http.StatusOK, 1},
		{"a room with no log", "/v1/rooms/zzzzzzzz/events", "sys:" + factory, http.StatusNotFound, 0},
		{"a bad room id", "/v1/rooms/NOPE/events", "sys:" + factory, http.StatusBadRequest, 0},
		{"a run token", "/v1/rooms/" + room + "/events", "run:" + runA, http.StatusUnauthorized, 0},
		{"no token", "/v1/rooms/" + room + "/events", "", http.StatusUnauthorized, 0},
	}
	for _, c := range reads {
		t.Run(c.name, func(t *testing.T) {
			rec := call(t, h, http.MethodGet, c.path, c.token, nil)
			var out struct {
				Events  []envelope.Event `json:"events"`
				LastSeq int64            `json:"lastSeq"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &out)
			if rec.Code != c.code || len(out.Events) != c.events || (c.code == http.StatusOK && out.LastSeq != 2) {
				t.Fatalf("%d %+v", rec.Code, out)
			}
		})
	}
}

// openStream dials the SSE stream as run's bridge and returns its line reader.
func openStream(t *testing.T, srv *httptest.Server, run string) (*bufio.Scanner, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/v1/bridge/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer run:"+run)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	return bufio.NewScanner(resp.Body), cancel
}

// ended reports whether the stream closes within a bound.
func ended(sc *bufio.Scanner) bool {
	done := make(chan struct{})
	go func() {
		for sc.Scan() {
		}
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(2 * time.Second):
		return false
	}
}

func expectPing(t *testing.T, sc *bufio.Scanner) {
	t.Helper()
	if !sc.Scan() || sc.Text() != ": ping" || !sc.Scan() || sc.Text() != "" {
		t.Fatalf("want a ping, got %q (%v)", sc.Text(), sc.Err())
	}
}

func TestStreamPingsAndClosesWhenTheRunEnds(t *testing.T) {
	s, _, w := newServer(t)
	tick := make(manualTicker, 1)
	s.Ticker = tick.new
	w.Upsert(t.Context(), agentRun(runA, room, "Running"))
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	sc, stop := openStream(t, srv, runA)
	defer stop()
	expectPing(t, sc)
	tick <- time.Now()
	expectPing(t, sc)
	w.Upsert(t.Context(), agentRun(runA, room, "Succeeded"))
	if !ended(sc) {
		t.Fatal("the stream outlived its run")
	}
}

// The last stream per run wins on a replica (§3).
func TestANewerStreamReplacesTheOlder(t *testing.T) {
	s, _, w := newServer(t)
	s.Ticker = make(manualTicker).new
	w.Upsert(t.Context(), agentRun(runA, room, "Running"))
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	older, stopOlder := openStream(t, srv, runA)
	defer stopOlder()
	expectPing(t, older)
	newer, stopNewer := openStream(t, srv, runA)
	defer stopNewer()
	expectPing(t, newer)
	if !ended(older) {
		t.Fatal("the older stream is still open")
	}
}

// The listener's ReadTimeout bounds reading a request, never a stream's life.
func TestStreamOutlivesTheReadTimeout(t *testing.T) {
	s, _, w := newServer(t)
	tick := make(manualTicker, 1)
	s.Ticker = tick.new
	w.Upsert(t.Context(), agentRun(runA, room, "Running"))
	srv := httptest.NewUnstartedServer(s.Routes())
	srv.Config.ReadTimeout = 50 * time.Millisecond
	srv.Start()
	defer srv.Close()
	sc, stop := openStream(t, srv, runA)
	defer stop()
	expectPing(t, sc)
	<-time.After(4 * srv.Config.ReadTimeout)
	tick <- time.Now()
	expectPing(t, sc)
}

// The stream ends with the bridge's token (§4): the bridge re-dials with a fresh one.
func TestStreamEndsWithTheToken(t *testing.T) {
	s, _, w := newServer(t)
	s.Ticker = make(manualTicker).new
	s.Runs = expiringAuth{expiry: time.Now().Add(100 * time.Millisecond)}
	w.Upsert(t.Context(), agentRun(runA, room, "Running"))
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	sc, stop := openStream(t, srv, runA)
	defer stop()
	expectPing(t, sc)
	if !ended(sc) {
		t.Fatal("the stream outlived its token")
	}
}

type expiringAuth struct{ expiry time.Time }

func (a expiringAuth) Authenticate(*http.Request) (authn.Principal, error) {
	return authn.Principal{Kind: envelope.ActorAgent, ID: "agent:" + runA, RunID: runA, Expiry: a.expiry}, nil
}

func TestShutdownClosesStreams(t *testing.T) {
	s, _, w := newServer(t)
	s.Ticker = make(manualTicker).new
	w.Upsert(t.Context(), agentRun(runA, room, "Running"))
	srv := httptest.NewServer(s.Routes())
	defer srv.Close()
	sc, stop := openStream(t, srv, runA)
	defer stop()
	expectPing(t, sc)
	s.closeStreams()
	if !ended(sc) {
		t.Fatal("the stream outlived shutdown")
	}
}
