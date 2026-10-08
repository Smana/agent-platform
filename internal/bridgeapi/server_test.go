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
	"net"
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
	"github.com/Smana/agent-platform/internal/fanout"
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
	// deliveryReads counts the replay's reads of the log.
	deliveryReads int
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

// LastAck is the store's: the highest ref the run acknowledged as delivered,
// interrupted, undeliverable or decision_applied that is a delivery of that run.
func (m *memLog) LastAck(_ context.Context, roomID, runID string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	evs := m.events[roomID]
	var last int64
	for _, e := range evs {
		var p struct {
			Kind string `json:"kind"`
			Ref  int64  `json:"ref"`
		}
		if e.RunID == runID && e.Type == envelope.StateChanged && json.Unmarshal(e.Payload, &p) == nil &&
			(p.Kind == "delivered" || p.Kind == "interrupted" || p.Kind == "undeliverable" || p.Kind == "decision_applied") && p.Ref >= 1 && p.Ref <= int64(len(evs)) {
			if _, _, ok := Deliverable(evs[p.Ref-1], runID); ok {
				last = max(last, p.Ref)
			}
		}
	}
	return last, nil
}

// Deliveries is the store's: the run's deliverable events in (after, through].
func (m *memLog) Deliveries(_ context.Context, roomID, runID string, after, through int64, limit int) ([]envelope.Event, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deliveryReads++
	var out []envelope.Event
	for _, e := range m.events[roomID] {
		if _, _, ok := Deliverable(e, runID); ok && e.Seq > after && e.Seq <= through && len(out) < limit {
			out = append(out, e)
		}
	}
	return out, nil
}

func (m *memLog) ClaimBridge(ctx context.Context, roomID, runID string, live func(context.Context, string) bool) (string, bool, error) {
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

// untouched fails the test when a refused request renewed the lease or wrote an event.
func (m *memLog) untouched(t *testing.T) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if n := len(m.events[room]); n != 0 || m.touches != 0 {
		t.Fatalf("a refused request wrote %d events and renewed the lease %d times", n, m.touches)
	}
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
	hub := fanout.New(log, nil, nil)
	hub.PollEvery = 10 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- hub.Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
	s := &Server{Log: log, Redactor: red, Runs: tokenAuth{"run:"}, Systems: tokenAuth{"sys:"}, Watch: w,
		Hub: hub, LastAck: log.LastAck, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
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
		{"a bridge's tool_result with the secret as a key", func(t *testing.T, h http.Handler) int {
			it := wire.Item{Stream: wire.StreamEvents, Seq: 4, Type: envelope.ToolResult,
				Payload: envelope.Must(map[string]any{"callId": "c", "status": "ok", "env": map[string]any{secret: "set"}})}
			return call(t, h, http.MethodPost, "/v1/bridge/events", "run:"+runA, batch(it)).Code
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
			log.untouched(t)
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

// Review I2: a quiet bridge's heartbeat is an empty batch. It renews the lease,
// fenced like an append: the holder gets a zero ack, a displaced bridge 409.
func TestAnEmptyBatchRenewsTheLeaseFenced(t *testing.T) {
	s, log, w := newServer(t)
	h := s.Routes()
	w.Upsert(t.Context(), agentRun(runA, room, "Running"))
	hello(t, h, runA)
	empty := []byte(`{"items":[]}`)
	rec := call(t, h, http.MethodPost, "/v1/bridge/events", "run:"+runA, empty)
	var ack wire.BatchAck
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &ack) != nil || ack != (wire.BatchAck{}) {
		t.Fatalf("holder's heartbeat: %d %s", rec.Code, rec.Body)
	}
	log.mu.Lock()
	touches := log.touches
	log.leases[room] = runB
	log.mu.Unlock()
	if touches != 1 {
		t.Fatalf("the heartbeat renewed the lease %d times", touches)
	}
	rec = call(t, h, http.MethodPost, "/v1/bridge/events", "run:"+runA, empty)
	if rec.Code != http.StatusConflict || reason(t, rec) != wire.ReasonLeaseLost {
		t.Fatalf("displaced heartbeat: %d %s", rec.Code, rec.Body)
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
		// Ruling AI (I1): a key spelled another way than the envelope's reaches Go
		// readers, which fold case, and not jsonb readers, which do not.
		{"a steering delivery behind a capital", wire.Item{Stream: wire.StreamEvents, Seq: 4, Type: envelope.Message,
			Payload: raw(`{"kind":"chat","Delivery":"steering","To":["agent:x"],"text":"hi"}`)}, wire.ReasonBadItem},
		{"a long-s key", wire.Item{Stream: wire.StreamEvents, Seq: 4, Type: envelope.Message,
			Payload: raw(`{"kind":"chat","delivery":"none","verdict":"x","ſverdict":"y"}`)}, wire.ReasonBadItem},
		{"a Kelvin-sign kind", wire.Item{Stream: wire.StreamEvents, Seq: 4, Type: envelope.Message,
			Payload: raw(`{"Kind":"review_verdict","kind":"chat","delivery":"none"}`)}, wire.ReasonBadItem},
		{"a Kelvin-sign kind alone", wire.Item{Stream: wire.StreamEvents, Seq: 4, Type: envelope.Message,
			Payload: raw(`{"Kind":"chat","delivery":"none"}`)}, wire.ReasonBadItem},
		{"a long-s status", wire.Item{Stream: wire.StreamStatus, Seq: 1, Type: envelope.StateChanged,
			Payload: raw(`{"kind":"harness_status","status":"running","ſtatus":"finished"}`)}, wire.ReasonBadItem},
		{"a state kind in capitals", wire.Item{Stream: wire.StreamStatus, Seq: 1, Type: envelope.StateChanged,
			Payload: raw(`{"KIND":"harness_status"}`)}, wire.ReasonBadItem},
		{"a tool result's status twice", wire.Item{Stream: wire.StreamEvents, Seq: 4, Type: envelope.ToolResult,
			Payload: raw(`{"callId":"c","status":"ok","ſtatus":"error"}`)}, wire.ReasonBadItem},
		{"a tool call's field misspelt", wire.Item{Stream: wire.StreamEvents, Seq: 4, Type: envelope.ToolCall,
			Payload: raw(`{"CallId":"c","tool":"bash"}`)}, wire.ReasonBadItem},
		{"a message field the envelope lacks", wire.Item{Stream: wire.StreamEvents, Seq: 4, Type: envelope.Message,
			Payload: raw(`{"kind":"chat","delivery":"none","priority":"high"}`)}, wire.ReasonBadItem},
		{"a state field in capitals", wire.Item{Stream: wire.StreamStatus, Seq: 1, Type: envelope.StateChanged,
			Payload: raw(`{"kind":"harness_error","Detail":"x"}`)}, wire.ReasonBadItem},
		{"a lone Reason", wire.Item{Stream: wire.StreamStatus, Seq: 1, Type: envelope.StateChanged,
			Payload: raw(`{"kind":"harness_status","Reason":"x"}`)}, wire.ReasonBadItem},
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
			log.untouched(t)
		})
	}
}

func TestABridgePushesWhatItsMappingProduces(t *testing.T) {
	var items []wire.Item
	for i, kind := range []string{"harness_status", "harness_error", "harness_paused", "harness_event",
		"delivered", "interrupted", "undeliverable", "policy_decision", "decision_applied"} {
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

// Ruling AI (I1): a message is stored as its envelope struct re-marshals it, so
// each key has one spelling whoever reads it.
// Ruling AI round 2: keys that are one once redacted are an honest env dump as
// often as an attack. That item alone is stored as a stub, so the bridge's
// cursor moves on and the rest of its batch is kept.
func TestACollidingItemBecomesAStub(t *testing.T) {
	cases := []struct {
		name    string
		payload json.RawMessage
		typ     envelope.Type
	}{
		{"two secret keys that merge", envelope.Must(map[string]any{"callId": "c", "env": map[string]any{
			"ghs_" + "Zq8mR2tXv9LkPw4NcYb7HsJ1fGdE6aUo3iTe": "a", "ghs_" + "Hb3nW8qLx2Rt7YvK9cPd4MzJ6sFgA1eUo5iN": "b"}}), envelope.ToolResult}, // pragma: allowlist secret
		{"a verdict behind a NUL in a key", json.RawMessage(`{"kind":"chat","ki\u0000nd":"review_verdict","delivery":"none"}`), envelope.Message},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, log, w := newServer(t)
			w.Upsert(t.Context(), agentRun(runA, room, "Running"))
			h := s.Routes()
			hello(t, h, runA)
			it := wire.Item{Stream: wire.StreamEvents, Seq: 2, Type: c.typ, Payload: c.payload}
			if rec := call(t, h, http.MethodPost, "/v1/bridge/events", "run:"+runA, batch(chat(1, "kept"), it, chat(3, "kept too"))); rec.Code != http.StatusOK {
				t.Fatalf("%d %s", rec.Code, rec.Body)
			}
			evs := log.stored()
			if len(evs) != 3 {
				t.Fatalf("%d events, want 3", len(evs))
			}
			stub := evs[1]
			// S1 review I-4: the stub says why, so the metric can.
			if got, want := string(stub.Payload), `{"reason":"key_collision","refused":true,"type":"`+string(c.typ)+`"}`; got != want || stub.Type != c.typ || len(stub.Redactions) != 0 {
				t.Fatalf("stub = %s %s %v, want %s", stub.Type, got, stub.Redactions, want)
			}
		})
	}
}

// Ruling AI round 2: a chat never carries a verdict's fields, whatever it sent.
func TestAChatCarriesNoVerdict(t *testing.T) {
	s, log, w := newServer(t)
	w.Upsert(t.Context(), agentRun(runA, room, "Running"))
	h := s.Routes()
	hello(t, h, runA)
	it := wire.Item{Stream: wire.StreamEvents, Seq: 1, Type: envelope.Message,
		Payload: json.RawMessage(`{"kind":"chat","text":"lgtm","delivery":"none","verdict":"approve","commit":"0123abc","pullRequest":"https://github.com/Smana/cloud-native-ref/pull/12"}`)}
	if rec := call(t, h, http.MethodPost, "/v1/bridge/events", "run:"+runA, batch(it)); rec.Code != http.StatusOK {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if got := string(log.stored()[0].Payload); strings.Contains(got, "verdict") || strings.Contains(got, "commit") || strings.Contains(got, "pull") ||
		got != `{"kind":"chat","text":"lgtm","delivery":"none"}` {
		t.Fatalf("stored %s", got)
	}
}

func TestAMessageIsStoredCanonically(t *testing.T) {
	s, log, w := newServer(t)
	w.Upsert(t.Context(), agentRun(runA, room, "Running"))
	h := s.Routes()
	hello(t, h, runA)
	it := wire.Item{Stream: wire.StreamEvents, Seq: 1, Type: envelope.Message,
		Payload: json.RawMessage(`{"text":"hi","kind":"chat","delivery":null}`)}
	if rec := call(t, h, http.MethodPost, "/v1/bridge/events", "run:"+runA, batch(it)); rec.Code != http.StatusOK {
		t.Fatalf("refused: %d %s", rec.Code, rec.Body)
	}
	if got, want := string(log.stored()[0].Payload), `{"kind":"chat","text":"hi","delivery":"none"}`; got != want {
		t.Fatalf("stored %s, want %s", got, want)
	}
}

// swapRedactor turns every payload into to: it isolates which payload the
// allow-check reads, the one a bridge sent or the one that is stored.
type swapRedactor struct{ to json.RawMessage }

func (r swapRedactor) Payload(context.Context, json.RawMessage) (json.RawMessage, []string, error) {
	return r.to, nil, nil
}

func TestTheAllowCheckReadsTheRedactedPayload(t *testing.T) {
	cases := []struct {
		name     string
		sent, to json.RawMessage
		code     int
	}{
		{"a chat that redaction turns into a verdict", envelope.Must(envelope.MessagePayload{Kind: envelope.KindChat, Delivery: envelope.DeliveryNone}),
			envelope.Must(envelope.MessagePayload{Kind: envelope.KindReviewVerdict, Verdict: "approve", Delivery: envelope.DeliveryNone}), http.StatusBadRequest},
		{"a verdict that redaction turns into a chat", envelope.Must(envelope.MessagePayload{Kind: envelope.KindReviewVerdict, Delivery: envelope.DeliveryNone}),
			envelope.Must(envelope.MessagePayload{Kind: envelope.KindChat, Delivery: envelope.DeliveryNone}), http.StatusOK},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, _, w := newServer(t)
			s.Redactor = swapRedactor{to: c.to}
			w.Upsert(t.Context(), agentRun(runA, room, "Running"))
			h := s.Routes()
			hello(t, h, runA)
			it := wire.Item{Stream: wire.StreamEvents, Seq: 1, Type: envelope.Message, Payload: c.sent}
			if rec := call(t, h, http.MethodPost, "/v1/bridge/events", "run:"+runA, batch(it)); rec.Code != c.code {
				t.Fatalf("%d %s, want %d", rec.Code, rec.Body, c.code)
			}
		})
	}
}

// An invalid type is attacker-sized text: the log carries a marker, never the value.
func TestAnInvalidTypeIsNotLogged(t *testing.T) {
	s, _, w := newServer(t)
	var out bytes.Buffer
	s.Logger = slog.New(slog.NewTextHandler(&out, nil))
	w.Upsert(t.Context(), agentRun(runA, room, "Running"))
	h := s.Routes()
	hello(t, h, runA)
	forged := strings.Repeat("Z", 4096)
	it := wire.Item{Stream: wire.StreamEvents, Seq: 1, Type: envelope.Type(forged), Payload: json.RawMessage(`{}`)}
	if rec := call(t, h, http.MethodPost, "/v1/bridge/events", "run:"+runA, batch(it)); rec.Code != http.StatusBadRequest {
		t.Fatalf("%d", rec.Code)
	}
	if strings.Contains(out.String(), "ZZZZ") || !strings.Contains(out.String(), "bridge item refused") {
		t.Fatalf("log: %.200s", out.String())
	}
}

// Ruling AI (3): the redaction honours the request's deadline, and a batch it
// could not finish is not written.
func TestAnEndedRequestWritesNothing(t *testing.T) {
	s, log, w := newServer(t)
	w.Upsert(t.Context(), agentRun(runA, room, "Running"))
	h := s.Routes()
	hello(t, h, runA)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	b, _ := json.Marshal(batch(chat(1, "x")))
	r := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/bridge/events", bytes.NewReader(b))
	r.Header.Set("Authorization", "Bearer run:"+runA)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	if rec.Code != http.StatusServiceUnavailable || reason(t, rec) != wire.ReasonTimedOut {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	log.untouched(t)
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
			log.untouched(t)
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
	if ev := log.stored()[0]; string(ev.Payload) != `{"reason":"invalid_value","refused":true,"type":"message"}` || ev.Type != envelope.Message {
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

func TestTaskFactsAppendAStateChanged(t *testing.T) {
	s, log, _ := newServer(t)
	h := s.Routes()
	path := "/v1/rooms/" + room + "/task"
	body := []byte(`{"clientSeq":7,"facts":{"phase":"Implementing","run":{"id":"cf4ato2x","role":"implementer"}}}`)
	rec := call(t, h, http.MethodPost, path, "sys:"+factory, body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("code %d: %s", rec.Code, rec.Body)
	}
	evs, _ := log.Range(t.Context(), room, 0, 10)
	if len(evs) != 1 || evs[0].Type != envelope.StateChanged || evs[0].Actor.Kind != envelope.ActorSystem {
		t.Fatalf("events %+v", evs)
	}
	var p map[string]any
	if err := json.Unmarshal(evs[0].Payload, &p); err != nil || p["kind"] != "task" || p["phase"] != "Implementing" {
		t.Fatalf("payload %s: %v", evs[0].Payload, err)
	}
	if rec := call(t, h, http.MethodPost, path, "sys:"+factory, body); rec.Code != http.StatusOK {
		t.Fatalf("replay code %d", rec.Code)
	}
	// Its own origin client: a task_state message with the same clientSeq is not a replay of the facts.
	msg := map[string]any{"kind": "task_state", "text": "x", "clientSeq": 7}
	if rec := call(t, h, http.MethodPost, "/v1/rooms/"+room+"/messages", "sys:"+factory, msg); rec.Code != http.StatusCreated {
		t.Fatalf("message after facts: %d", rec.Code)
	}
}

func TestTaskFactsRefuseBadRequests(t *testing.T) {
	s, _, _ := newServer(t)
	h := s.Routes()
	for _, body := range []string{
		`{"clientSeq":1,"facts":{}}`,
		`{"clientSeq":0,"facts":{"phase":"Queued"}}`,
		`{"clientSeq":1,"facts":{"phase":"Queued","pr":{"number":1,"url":"https://evil.example/pull/1"}}}`,
		`{"clientSeq":1,"facts":{"phase":"Queued"},"extra":true}`,
	} {
		rec := call(t, h, http.MethodPost, "/v1/rooms/"+room+"/task", "sys:"+factory, []byte(body))
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: code %d", body, rec.Code)
		}
	}
	for name, c := range map[string]struct {
		path, token string
		code        int
	}{
		"a bad room":  {"/v1/rooms/NOPE/task", "sys:" + factory, http.StatusBadRequest},
		"a run token": {"/v1/rooms/" + room + "/task", "run:" + runA, http.StatusUnauthorized},
		"an unlisted": {"/v1/rooms/" + room + "/task", "sys:forbidden", http.StatusForbidden},
		"no room log": {"/v1/rooms/zzzzzzzz/task", "sys:" + factory, http.StatusNotFound},
	} {
		rec := call(t, h, http.MethodPost, c.path, c.token, []byte(`{"clientSeq":1,"facts":{"phase":"Queued"}}`))
		if rec.Code != c.code {
			t.Errorf("%s: code %d, want %d", name, rec.Code, c.code)
		}
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

// F10: Go's HTTP/2 server enforces a write deadline as a per-stream timer that
// resets the stream when it fires, write pending or not. A stream idle past
// StreamWriteWait, between two pings, must stay open.
func TestAnIdleHTTP2StreamOutlivesTheWriteWait(t *testing.T) {
	s, _, w := newServer(t)
	tick := make(manualTicker, 1)
	s.Ticker = tick.new
	s.StreamWriteWait = 50 * time.Millisecond
	w.Upsert(t.Context(), agentRun(runA, room, "Running"))
	srv := httptest.NewUnstartedServer(s.Routes())
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/v1/bridge/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer run:"+runA)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.ProtoMajor != 2 || resp.StatusCode != http.StatusOK {
		t.Fatalf("want an HTTP/2 stream, got %s %d", resp.Proto, resp.StatusCode)
	}
	sc := bufio.NewScanner(resp.Body)
	expectPing(t, sc)
	for range 2 {
		<-time.After(6 * s.StreamWriteWait)
		tick <- time.Now()
		expectPing(t, sc)
	}
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

// Ruling AI (3): each principal has a request rate and a bound on requests in flight.
func TestEachPrincipalIsRateLimited(t *testing.T) {
	cases := []struct {
		name, method, path, token string
		body                      func(n int) any
	}{
		{"a bridge's events", http.MethodPost, "/v1/bridge/events", "run:" + runA, func(n int) any { return batch(chat(int64(n), "x")) }},
		{"a bridge's hello", http.MethodPost, "/v1/bridge/hello", "run:" + runA, func(int) any { return nil }},
		{"a system read", http.MethodGet, "/v1/rooms/" + room + "/events", "sys:" + factory, func(int) any { return nil }},
		{"a system write", http.MethodPost, "/v1/rooms/" + room + "/messages", "sys:" + factory,
			func(n int) any { return map[string]any{"kind": "task_state", "text": "x", "clientSeq": n} }},
		{"a system queue write", http.MethodPost, "/v1/rooms/" + room + "/queue", "sys:" + factory,
			func(n int) any { return map[string]any{"text": "x", "clientSeq": n} }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s, _, w := newServer(t)
			now := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
			s.Now = func() time.Time { return now }
			s.Limits = Limits{Rate: 1, Burst: 2, InFlight: 4}
			s.Queue = newMemQueue()
			w.Upsert(t.Context(), agentRun(runA, room, "Running"))
			h := s.Routes()
			hello(t, h, runA)
			now = now.Add(time.Minute) // the setup's hello drew on the run's bucket
			for n := 1; n <= 2; n++ {
				if rec := call(t, h, c.method, c.path, c.token, c.body(n)); rec.Code >= 300 {
					t.Fatalf("request %d within the burst: %d %s", n, rec.Code, rec.Body)
				}
			}
			rec := call(t, h, c.method, c.path, c.token, c.body(3))
			if rec.Code != http.StatusTooManyRequests || reason(t, rec) != wire.ReasonRateLimited || rec.Header().Get("Retry-After") == "" {
				t.Fatalf("over the burst: %d %s", rec.Code, rec.Body)
			}
			// Another principal has its own bucket.
			if rec := call(t, h, http.MethodGet, "/v1/rooms/"+room+"/events", "sys:system:other", nil); rec.Code != http.StatusOK {
				t.Fatalf("another principal: %d", rec.Code)
			}
			now = now.Add(time.Second)
			if rec := call(t, h, c.method, c.path, c.token, c.body(3)); rec.Code >= 300 {
				t.Fatalf("a second later: %d %s", rec.Code, rec.Body)
			}
		})
	}
}

func TestRequestsInFlightAreBounded(t *testing.T) {
	s, log, w := newServer(t)
	s.Limits = Limits{Rate: 1000, Burst: 1000, InFlight: 1}
	w.Upsert(t.Context(), agentRun(runA, room, "Running"))
	h := s.Routes()
	hello(t, h, runA)
	entered, gate := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	defer release() // a failing test must not leave the first request parked
	log.refuse = func(envelope.Draft) error {
		close(entered)
		<-gate
		return nil
	}
	first, second := make(chan int, 1), make(chan *httptest.ResponseRecorder, 1)
	go func() {
		first <- call(t, h, http.MethodPost, "/v1/bridge/events", "run:"+runA, batch(chat(1, "slow"))).Code
	}()
	<-entered
	go func() { second <- call(t, h, http.MethodPost, "/v1/bridge/events", "run:"+runA, batch(chat(2, "x"))) }()
	select {
	case rec := <-second:
		if rec.Code != http.StatusTooManyRequests || reason(t, rec) != wire.ReasonRateLimited {
			t.Fatalf("second in flight: %d %s", rec.Code, rec.Body)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a second request in flight was admitted")
	}
	release()
	if code := <-first; code != http.StatusOK {
		t.Fatalf("first: %d", code)
	}
	log.refuse = nil
	if rec := call(t, h, http.MethodPost, "/v1/bridge/events", "run:"+runA, batch(chat(2, "x"))); rec.Code != http.StatusOK {
		t.Fatalf("after the first ended: %d", rec.Code)
	}
}

// smallBuffers shrinks each accepted connection's send buffer, so a stream
// nobody reads blocks after a few kilobytes.
type smallBuffers struct{ net.Listener }

func (l smallBuffers) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetWriteBuffer(1 << 10)
	}
	return c, err
}

// A bridge that stops reading must not pin its handler until the token
// expires: each write has its own deadline.
func TestAStreamNobodyReadsEnds(t *testing.T) {
	s, _, w := newServer(t)
	tick := make(manualTicker)
	s.Ticker = tick.new
	s.StreamWriteWait = 100 * time.Millisecond
	w.Upsert(t.Context(), agentRun(runA, room, "Running"))
	h := s.Routes()
	ended := make(chan struct{})
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r)
		close(ended)
	}))
	srv.Listener = smallBuffers{srv.Listener}
	srv.Start()
	defer srv.Close()
	var d net.Dialer
	conn, err := d.DialContext(t.Context(), "tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.(*net.TCPConn).SetReadBuffer(1 << 10)
	if _, err := fmt.Fprintf(conn, "GET /v1/bridge/stream HTTP/1.1\r\nHost: broker\r\nAuthorization: Bearer run:%s\r\n\r\n", runA); err != nil {
		t.Fatal(err)
	}
	// Never read: each tick writes a ping until the buffers fill and a write blocks.
	deadline := time.After(10 * time.Second)
	for {
		select {
		case <-ended:
			return
		case tick <- time.Now():
		case <-deadline:
			t.Fatal("the handler outlived a bridge that stopped reading")
		}
	}
}
