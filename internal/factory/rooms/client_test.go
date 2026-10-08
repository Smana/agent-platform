// SPDX-License-Identifier: Apache-2.0

package rooms

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/attribute"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/httpx"
	"github.com/Smana/agent-platform/internal/wire"
)

// broker is the system API of internal/bridgeapi (roomEvents, roomMessage) as it answers: the
// events reply is {"events","lastSeq"}, a message is strictly {kind, text, clientSeq}, and a
// refusal is wire.Error.
type broker struct {
	t      *testing.T
	mu     sync.Mutex
	log    []envelope.Event
	auth   []string
	trace  []string
	bodies []string
	fail   map[string]int // path → status to answer with, and its wire reason below
	reason string
	limits []string
	lie    func(evs []envelope.Event) []envelope.Event
	// The queue routes: what the broker lists, and every consume's refs.
	queued   []Queued
	consumes [][]int64
}

func newBroker(t *testing.T, n int) (*broker, *httptest.Server) {
	b := &broker{t: t, fail: map[string]int{}}
	for i := 1; i <= n; i++ {
		b.log = append(b.log, envelope.Event{Seq: int64(i), RoomID: "3buqdlot", Type: envelope.Message})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/rooms/{id}/events", func(w http.ResponseWriter, r *http.Request) {
		if b.refuse(w, r) {
			return
		}
		after, _ := strconv.ParseInt(r.URL.Query().Get("afterSeq"), 10, 64)
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		b.mu.Lock()
		b.limits = append(b.limits, r.URL.Query().Get("limit"))
		var out []envelope.Event
		for _, e := range b.log {
			if e.Seq > after && len(out) < limit {
				out = append(out, e)
			}
		}
		last := int64(len(b.log))
		lie := b.lie
		b.mu.Unlock()
		if lie != nil {
			out = lie(out)
		}
		if out == nil {
			out = []envelope.Event{}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"events": out, "lastSeq": last})
	})
	mux.HandleFunc("POST /v1/rooms/{id}/messages", func(w http.ResponseWriter, r *http.Request) {
		if b.refuse(w, r) {
			return
		}
		raw, _ := io.ReadAll(r.Body)
		b.mu.Lock()
		b.bodies = append(b.bodies, string(raw))
		b.mu.Unlock()
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		var in struct {
			Kind      envelope.MessageKind `json:"kind"`
			Text      string               `json:"text"`
			ClientSeq int64                `json:"clientSeq"`
		}
		if err := dec.Decode(&in); err != nil || in.Kind != envelope.KindTaskState || in.ClientSeq <= 0 {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(wire.Error{Reason: wire.ReasonBadMessage})
			return
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"seq":9}`)
	})
	mux.HandleFunc("POST /v1/rooms/{id}/task", func(w http.ResponseWriter, r *http.Request) {
		if b.refuse(w, r) {
			return
		}
		raw, _ := io.ReadAll(r.Body)
		b.mu.Lock()
		b.bodies = append(b.bodies, string(raw))
		b.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"seq":10}`)
	})
	mux.HandleFunc("POST /v1/rooms/{id}/queue", func(w http.ResponseWriter, r *http.Request) {
		if b.refuse(w, r) {
			return
		}
		raw, _ := io.ReadAll(r.Body)
		b.mu.Lock()
		b.bodies = append(b.bodies, string(raw))
		b.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"seq":4}`)
	})
	mux.HandleFunc("GET /v1/rooms/{id}/queue", func(w http.ResponseWriter, r *http.Request) {
		if b.refuse(w, r) {
			return
		}
		b.mu.Lock()
		defer b.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"queued": b.queued})
	})
	mux.HandleFunc("POST /v1/rooms/{id}/queue/consume", func(w http.ResponseWriter, r *http.Request) {
		if b.refuse(w, r) {
			return
		}
		var in struct {
			Refs  []int64 `json:"refs"`
			RunID string  `json:"runId"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		b.mu.Lock()
		b.consumes = append(b.consumes, in.Refs)
		b.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]int{"consumed": len(in.Refs)})
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return b, srv
}

func (b *broker) refuse(w http.ResponseWriter, r *http.Request) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.auth = append(b.auth, r.Header.Get("Authorization"))
	b.trace = append(b.trace, r.Header.Get("Traceparent"))
	if code, ok := b.fail[r.URL.Path]; ok {
		if code == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "1")
		}
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(wire.Error{Reason: b.reason})
		return true
	}
	return false
}

type rig struct {
	c     *Client
	b     *broker
	tok   string
	spans *tracetest.SpanRecorder
}

func newRig(t *testing.T, events int) *rig {
	t.Helper()
	b, srv := newBroker(t, events)
	tok := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tok, []byte("t1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	spans := tracetest.NewSpanRecorder()
	c, err := New(srv.URL, tok, httpx.New(5*time.Second, roots), sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(spans)))
	if err != nil {
		t.Fatal(err)
	}
	return &rig{c: c, b: b, tok: tok, spans: spans}
}

func TestEventsSinceReadsEveryPageWithTheTokenOfTheMoment(t *testing.T) {
	r := newRig(t, 2*pageSize+3)
	evs, cursor, err := r.c.EventsSince(t.Context(), "3buqdlot", 1)
	if err != nil || len(evs) != 2*pageSize+2 || cursor != int64(2*pageSize+3) || evs[0].Seq != 2 {
		t.Fatalf("%d events, cursor %d, %v", len(evs), cursor, err)
	}
	if len(r.b.limits) != 3 || r.b.limits[0] != strconv.Itoa(pageSize) {
		t.Fatalf("pages %v", r.b.limits)
	}
	if r.b.auth[0] != "Bearer t1" {
		t.Fatalf("auth %q", r.b.auth[0])
	}
	if err := os.WriteFile(r.tok, []byte("t2\n"), 0o600); err != nil { // kubelet rotated it
		t.Fatal(err)
	}
	if err := r.c.TaskState(t.Context(), "3buqdlot", "Implementing", 1); err != nil {
		t.Fatal(err)
	}
	if got := r.b.auth[len(r.b.auth)-1]; got != "Bearer t2" {
		t.Fatalf("the token is re-read before every call: %q", got)
	}
	if body := r.b.bodies[0]; body != `{"clientSeq":1,"kind":"task_state","text":"Implementing"}` {
		t.Fatalf("the body the broker decodes strictly: %s", body)
	}
}

// EventsSince stops at a hundred pages and returns the cursor it reached, never the room's
// lastSeq: resuming from lastSeq would skip what it did not read.
func TestEventsSinceStopsAtItsCapWithAResumableCursor(t *testing.T) {
	r := newRig(t, maxPages*pageSize+5)
	evs, cursor, err := r.c.EventsSince(t.Context(), "3buqdlot", 0)
	if err != nil || len(evs) != maxPages*pageSize || cursor != int64(maxPages*pageSize) {
		t.Fatalf("%d events, cursor %d, %v", len(evs), cursor, err)
	}
	evs, cursor, err = r.c.EventsSince(t.Context(), "3buqdlot", cursor)
	if err != nil || len(evs) != 5 || cursor != int64(maxPages*pageSize+5) {
		t.Fatalf("%d events, cursor %d, %v", len(evs), cursor, err)
	}
	if evs, cursor, err := r.c.EventsSince(t.Context(), "3buqdlot", cursor); err != nil || len(evs) != 0 || cursor != int64(maxPages*pageSize+5) {
		t.Fatalf("nothing new keeps the cursor: %d %d %v", len(evs), cursor, err)
	}
}

func TestEventsReturnsTheRoomsLastSeq(t *testing.T) {
	r := newRig(t, 7)
	evs, last, err := r.c.Events(t.Context(), "3buqdlot", 0, 1)
	if err != nil || len(evs) != 1 || last != 7 {
		t.Fatalf("%v %d %v", evs, last, err)
	}
}

// A reply that would move the cursor wrong is refused: events at or before afterSeq, out of
// order, of another room, or more than asked for.
func TestEventsRefusesAReplyThatWouldMisplaceTheCursor(t *testing.T) {
	for name, lie := range map[string]func([]envelope.Event) []envelope.Event{
		"at or before afterSeq": func(evs []envelope.Event) []envelope.Event { evs[0].Seq = 1; return evs },
		"out of order":          func(evs []envelope.Event) []envelope.Event { slices.Reverse(evs); return evs },
		"a repeated seq":        func(evs []envelope.Event) []envelope.Event { evs[1] = evs[0]; return evs },
		"another room":          func(evs []envelope.Event) []envelope.Event { evs[0].RoomID = "aaaaaaaa"; return evs },
		"more than asked": func(evs []envelope.Event) []envelope.Event {
			return append(evs, envelope.Event{Seq: 99, RoomID: "3buqdlot"})
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, 5)
			r.b.lie = lie
			if _, _, err := r.c.Events(t.Context(), "3buqdlot", 1, 3); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

// A reply is read to a bound: a page's events at the C4 payload cap, and a small one for a message.
func TestAnOversizeReplyIsRefused(t *testing.T) {
	r := newRig(t, 3)
	huge := json.RawMessage(`"` + strings.Repeat("x", 4*eventBytes) + `"`)
	r.b.lie = func(evs []envelope.Event) []envelope.Event { evs[0].Payload = huge; return evs }
	if _, _, err := r.c.Events(t.Context(), "3buqdlot", 0, 3); !errors.Is(err, httpx.ErrBodyTooLarge) {
		t.Fatalf("err = %v", err)
	}
	r.b.lie = func(evs []envelope.Event) []envelope.Event {
		evs[0].Payload = json.RawMessage(`"` + strings.Repeat("x", eventBytes-1024) + `"`)
		return evs
	}
	if _, _, err := r.c.Events(t.Context(), "3buqdlot", 0, 3); err != nil {
		t.Fatalf("a payload at the cap: %v", err)
	}
}

func TestTheBrokersRefusalIsTyped(t *testing.T) {
	for _, c := range []struct {
		code   int
		reason string
	}{
		{http.StatusNotFound, wire.ReasonNoRoom},
		{http.StatusGone, wire.ReasonSealed},
		{http.StatusTooManyRequests, wire.ReasonRateLimited},
		{http.StatusForbidden, wire.ReasonNotPermitted},
		{http.StatusServiceUnavailable, wire.ReasonLogUnavailable},
	} {
		t.Run(c.reason, func(t *testing.T) {
			r := newRig(t, 1)
			r.b.fail["/v1/rooms/3buqdlot/events"], r.b.fail["/v1/rooms/3buqdlot/messages"], r.b.reason = c.code, c.code, c.reason
			_, _, err := r.c.Events(t.Context(), "3buqdlot", 0, 10)
			var api *APIError
			if !errors.As(err, &api) || api.Status != c.code || api.Reason != c.reason {
				t.Fatalf("events: %v", err)
			}
			err = r.c.TaskState(t.Context(), "3buqdlot", "x", 1)
			if !errors.As(err, &api) || api.Status != c.code || api.Reason != c.reason {
				t.Fatalf("task_state: %v", err)
			}
			// Callers branch on these three: no_room is a retry, not_permitted the missing FR-1 entry,
			// sealed a room that takes no event again.
			if errors.Is(err, ErrNoRoom) != (c.reason == wire.ReasonNoRoom) ||
				errors.Is(err, ErrNotPermitted) != (c.reason == wire.ReasonNotPermitted) ||
				errors.Is(err, ErrSealed) != (c.reason == wire.ReasonSealed) {
				t.Fatalf("errors.Is: %v", err)
			}
		})
	}
}

// What the broker would refuse is refused here, before a request: the broker clamps a limit
// above 500 to 100 without saying so, and answers 400 to the rest.
func TestTheClientRefusesWhatTheBrokerWould(t *testing.T) {
	r := newRig(t, 1)
	ctx := t.Context()
	for name, call := range map[string]func() error{
		"a room that is not a C2 id": func() error { _, _, err := r.c.Events(ctx, "NOTANID1", 0, 10); return err },
		"a negative afterSeq":        func() error { _, _, err := r.c.Events(ctx, "3buqdlot", -1, 10); return err },
		"a zero limit":               func() error { _, _, err := r.c.Events(ctx, "3buqdlot", 0, 0); return err },
		"a limit over 500":           func() error { _, _, err := r.c.Events(ctx, "3buqdlot", 0, 501); return err },
		"since in a bad room":        func() error { _, _, err := r.c.EventsSince(ctx, "../x", 0); return err },
		"a message to a bad room":    func() error { return r.c.TaskState(ctx, "3BUQDLOT", "x", 1) },
		"a zero clientSeq":           func() error { return r.c.TaskState(ctx, "3buqdlot", "x", 0) },
		"a message over 16 KiB": func() error {
			return r.c.TaskState(ctx, "3buqdlot", strings.Repeat("x", envelope.MaxHumanMessage+1), 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); err == nil {
				t.Fatal("sent")
			}
		})
	}
	if len(r.b.auth) != 0 {
		t.Fatalf("%d requests reached the broker", len(r.b.auth))
	}
	if err := r.c.TaskState(ctx, "3buqdlot", strings.Repeat("x", envelope.MaxHumanMessage), 1); err != nil {
		t.Fatalf("16 KiB is the broker's own cap: %v", err)
	}
	if _, _, err := r.c.Events(ctx, "3buqdlot", 0, 500); err != nil {
		t.Fatalf("500 is the broker's own cap: %v", err)
	}
}

func TestAnEmptyOrMissingTokenIsNeverSent(t *testing.T) {
	r := newRig(t, 1)
	if err := os.WriteFile(r.tok, []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.c.Events(t.Context(), "3buqdlot", 0, 10); err == nil {
		t.Fatal("an empty token")
	}
	if err := os.Remove(r.tok); err != nil {
		t.Fatal(err)
	}
	if err := r.c.TaskState(t.Context(), "3buqdlot", "x", 1); err == nil {
		t.Fatal("no token")
	}
	if len(r.b.auth) != 0 {
		t.Fatalf("%d requests reached the broker", len(r.b.auth))
	}
}

// Ruling SC: the broker's :8443 is TLS, verified against the mounted CA; nothing else is dialled.
func TestNewRefusesAnythingButHTTPS(t *testing.T) {
	hc := httpx.New(time.Second, nil)
	for name, c := range map[string]struct {
		url, token string
		hc         *http.Client
	}{
		"http":           {"http://room-broker.agent-system.svc.cluster.local:8443", "/t", hc},
		"no host":        {"https://", "/t", hc},
		"userinfo":       {"https://u:p@room-broker.agent-system.svc.cluster.local:8443", "/t", hc},
		"a path":         {"https://room-broker.agent-system.svc.cluster.local:8443/v1", "/t", hc},
		"a query":        {"https://room-broker.agent-system.svc.cluster.local:8443?x=1", "/t", hc},
		"no token file":  {"https://room-broker.agent-system.svc.cluster.local:8443", "", hc},
		"no HTTP client": {"https://room-broker.agent-system.svc.cluster.local:8443", "/t", nil},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := New(c.url, c.token, c.hc, nil); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	if _, err := New("https://room-broker.agent-system.svc.cluster.local:8443/", "/t", hc, nil); err != nil {
		t.Fatalf("the broker's URL: %v", err)
	}
}

func TestLoadCAReadsAPEMBundle(t *testing.T) {
	_, srv := newBroker(t, 0)
	pemCert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	good := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(good, pemCert, 0o600); err != nil {
		t.Fatal(err)
	}
	if roots, err := LoadCA(good); err != nil || roots == nil {
		t.Fatalf("%v", err)
	}
	bad := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(bad, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadCA(bad); err == nil {
		t.Fatal("a file with no certificate is refused")
	}
	if _, err := LoadCA(filepath.Join(t.TempDir(), "absent")); err == nil {
		t.Fatal("a missing CA is refused")
	}
}

// O-1 M3: factory spans enter an unfiltered pipeline, so they carry metadata only: the room id,
// the method, the route template, the status and the broker's refusal reason. Never the text, the
// token or the query. The trace continues at the broker through traceparent.
func TestSpansCarryMetadataOnly(t *testing.T) {
	r := newRig(t, 3)
	ctx := t.Context()
	if _, _, err := r.c.EventsSince(ctx, "3buqdlot", 0); err != nil {
		t.Fatal(err)
	}
	if err := r.c.TaskState(ctx, "3buqdlot", "a secret-looking text", 1); err != nil {
		t.Fatal(err)
	}
	r.b.fail["/v1/rooms/3buqdlot/messages"], r.b.reason = http.StatusGone, wire.ReasonSealed
	if err := r.c.TaskState(ctx, "3buqdlot", "a secret-looking text", 2); err == nil {
		t.Fatal("sealed")
	}
	allowed := []attribute.Key{"agent.room_id", "http.request.method", "url.template", "http.response.status_code", "error.type"}
	ended := r.spans.Ended()
	if len(ended) != 3 {
		t.Fatalf("%d spans", len(ended))
	}
	for i, s := range ended {
		var keys []attribute.Key
		for _, kv := range s.Attributes() {
			keys = append(keys, kv.Key)
			if v := kv.Value.String(); strings.Contains(v, "secret") || strings.Contains(v, "t1") || strings.Contains(v, "?") {
				t.Errorf("%s: %s = %q", s.Name(), kv.Key, v)
			}
		}
		want := allowed[:4]
		if i == 2 {
			want = allowed
		}
		if !slices.Equal(keys, want) {
			t.Errorf("%s: attributes %v, want %v", s.Name(), keys, want)
		}
		if s.Status().Description != "" && s.Status().Description != wire.ReasonSealed {
			t.Errorf("%s: status %q", s.Name(), s.Status().Description)
		}
		if len(s.Events()) != 0 {
			t.Errorf("%s: events %v", s.Name(), s.Events())
		}
	}
	if n := ended[0].Name(); n != "broker GET /v1/rooms/{id}/events" || ended[1].Name() != "broker POST /v1/rooms/{id}/messages" {
		t.Errorf("names %q %q", n, ended[1].Name())
	}
	if got := ended[2].Attributes(); fmt.Sprint(got[4].Value.AsString()) != wire.ReasonSealed {
		t.Errorf("error.type %v", got)
	}
	for i, tp := range r.b.trace {
		if want := ended[i].SpanContext().TraceID().String(); !strings.Contains(tp, want) {
			t.Errorf("request %d: traceparent %q, want trace %s", i, tp, want)
		}
	}
}

// The reason comes from the peer's reply body: on a span it is bounded to the broker's own
// vocabulary, anything else read as other (review minor).
func TestASpanReasonIsBounded(t *testing.T) {
	for _, c := range []struct{ reason, want string }{
		{wire.ReasonBadRoom, wire.ReasonBadRoom},
		{wire.ReasonBadMessage, wire.ReasonBadMessage},
		{wire.ReasonUnauthenticated, wire.ReasonUnauthenticated},
		{wire.ReasonNotPermitted, wire.ReasonNotPermitted},
		{wire.ReasonNoRoom, wire.ReasonNoRoom},
		{wire.ReasonSealed, wire.ReasonSealed},
		{wire.ReasonRateLimited, wire.ReasonRateLimited},
		{wire.ReasonLogUnavailable, wire.ReasonLogUnavailable},
		{wire.ReasonTimedOut, wire.ReasonTimedOut},
		{wire.ReasonBadBatch, "other"}, // a bridge-API reason: never the system API's
		{"secret: ghp_0123456789", "other"},
		{"", "other"},
	} {
		r := newRig(t, 1)
		r.b.fail["/v1/rooms/3buqdlot/messages"], r.b.reason = http.StatusBadRequest, c.reason
		err := r.c.TaskState(t.Context(), "3buqdlot", "x", 1)
		var api *APIError
		if !errors.As(err, &api) || api.Reason != c.reason {
			t.Fatalf("callers still get the reply's reason: %v", err)
		}
		s := r.spans.Ended()[0]
		var got string
		for _, kv := range s.Attributes() {
			if kv.Key == "error.type" {
				got = kv.Value.AsString()
			}
		}
		if got != c.want || s.Status().Description != c.want {
			t.Errorf("reason %q: error.type %q, status %q, want %q", c.reason, got, s.Status().Description, c.want)
		}
	}
}

// SP3 R9: the factory queues a review on its own stream, lists the queue and consumes only what
// it is given, in batches the broker accepts.
func TestQueueCalls(t *testing.T) {
	r := newRig(t, 0)
	ctx := t.Context()
	if err := r.c.Enqueue(ctx, "3buqdlot", "review", "use the relative link", 901); err != nil {
		t.Fatal(err)
	}
	if body := r.b.bodies[0]; body != `{"text":"use the relative link","clientSeq":901,"stream":"review"}` {
		t.Fatalf("the body the broker decodes strictly: %s", body)
	}
	r.b.queued = []Queued{{Ref: 4, Author: "system:factory", Text: "use the relative link"}, {Ref: 9, Author: "system:factory", Text: "b"}}
	q, err := r.c.Queue(ctx, "3buqdlot")
	if err != nil || len(q) != 2 || q[0] != r.b.queued[0] {
		t.Fatalf("%v %v", q, err)
	}
	if err := r.c.Consume(ctx, "3buqdlot", nil, "aaaaaaaa"); err != nil || len(r.b.consumes) != 0 {
		t.Fatalf("consuming nothing calls nothing: %v %v", r.b.consumes, err)
	}
	refs := make([]int64, 2*maxConsume+1)
	for i := range refs {
		refs[i] = int64(i + 1)
	}
	if err := r.c.Consume(ctx, "3buqdlot", refs, "aaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	if len(r.b.consumes) != 3 || len(r.b.consumes[0]) != maxConsume || len(r.b.consumes[2]) != 1 || r.b.consumes[2][0] != refs[2*maxConsume] {
		t.Fatalf("batches of %d: %v", maxConsume, len(r.b.consumes))
	}
	// A listing out of order would make a caller quote, then consume, the wrong messages.
	for name, l := range map[string][]Queued{"out of order": {{Ref: 9}, {Ref: 4}}, "a repeated ref": {{Ref: 4}, {Ref: 4}},
		"a zero ref": {{Ref: 0}}} {
		r.b.queued = l
		if _, err := r.c.Queue(ctx, "3buqdlot"); err == nil {
			t.Fatalf("a listing with %s", name)
		}
	}
	r.b.fail["/v1/rooms/3buqdlot/queue"], r.b.reason = http.StatusNotImplemented, wire.ReasonNoQueue
	var api *APIError
	if err := r.c.Enqueue(ctx, "3buqdlot", "review", "x", 902); !errors.As(err, &api) || api.Reason != wire.ReasonNoQueue {
		t.Fatalf("a broker without queue routes: %v", err)
	}
}

func TestQueueCallsRefuseWhatTheBrokerWould(t *testing.T) {
	r := newRig(t, 0)
	ctx := t.Context()
	for name, call := range map[string]func() error{
		"a bad room":            func() error { return r.c.Enqueue(ctx, "3BUQDLOT", "review", "x", 1) },
		"a stream with a colon": func() error { return r.c.Enqueue(ctx, "3buqdlot", "a:b", "x", 1) },
		"an empty stream":       func() error { return r.c.Enqueue(ctx, "3buqdlot", "", "x", 1) },
		"a zero clientSeq":      func() error { return r.c.Enqueue(ctx, "3buqdlot", "review", "x", 0) },
		"a blank text":          func() error { return r.c.Enqueue(ctx, "3buqdlot", "review", " \n", 1) },
		"a text over 16 KiB": func() error {
			return r.c.Enqueue(ctx, "3buqdlot", "review", strings.Repeat("x", envelope.MaxHumanMessage+1), 1)
		},
		"a list of a bad room":  func() error { _, err := r.c.Queue(ctx, "../x"); return err },
		"a consume in bad room": func() error { return r.c.Consume(ctx, "../x", []int64{1}, "aaaaaaaa") },
		"a bad run id":          func() error { return r.c.Consume(ctx, "3buqdlot", []int64{1}, "NOPE") },
	} {
		t.Run(name, func(t *testing.T) {
			if err := call(); err == nil {
				t.Fatal("sent")
			}
		})
	}
	if len(r.b.auth) != 0 {
		t.Fatalf("%d requests reached the broker", len(r.b.auth))
	}
}

func TestTaskFactsPostsToTheTaskRoute(t *testing.T) {
	r := newRig(t, 0)
	f := envelope.TaskFacts{Phase: "Implementing", Run: &envelope.RunFact{ID: "cf4ato2x", Role: "implementer"}}
	// The fake mounts only POST /v1/rooms/{id}/task for this body, so a wrong path fails the call.
	if err := r.c.TaskFacts(t.Context(), "3buqdlot", f, 7); err != nil {
		t.Fatal(err)
	}
	if body := r.b.bodies[len(r.b.bodies)-1]; body != `{"clientSeq":7,"facts":{"phase":"Implementing","run":{"id":"cf4ato2x","role":"implementer"}}}` {
		t.Fatalf("body %s", body)
	}
}

// A broker older than the factory serves no task route (v0.7): its mux answers a plain-text 404, or a
// 405 when the path has another method's route, with no wire reason. A broker's own no_room is not it.
func TestAnUnservedRouteIsErrNoRoute(t *testing.T) {
	f := envelope.TaskFacts{Phase: "Queued"}
	served := newRig(t, 0)
	served.b.fail["/v1/rooms/3buqdlot/task"], served.b.reason = http.StatusNotFound, wire.ReasonNoRoom
	if err := served.c.TaskFacts(t.Context(), "3buqdlot", f, 1); !errors.Is(err, ErrNoRoom) || errors.Is(err, ErrNoRoute) {
		t.Fatalf("no_room: %v", err)
	}
	for name, mux := range map[string]*http.ServeMux{"404": http.NewServeMux(), "405": http.NewServeMux()} {
		if name == "405" {
			mux.HandleFunc("GET /v1/rooms/{id}/task", func(http.ResponseWriter, *http.Request) {})
		}
		srv := httptest.NewTLSServer(mux)
		t.Cleanup(srv.Close)
		roots := x509.NewCertPool()
		roots.AddCert(srv.Certificate())
		c, err := New(srv.URL, served.tok, httpx.New(5*time.Second, roots), nil)
		if err != nil {
			t.Fatal(err)
		}
		err = c.TaskFacts(t.Context(), "3buqdlot", f, 1)
		if !errors.Is(err, ErrNoRoute) || errors.Is(err, ErrNoRoom) {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestTaskFactsRefusesBeforeARequest(t *testing.T) {
	r := newRig(t, 0)
	ok := envelope.TaskFacts{Phase: "Queued"}
	for name, err := range map[string]error{
		"a bad room":       r.c.TaskFacts(t.Context(), "3BUQDLOT", ok, 1),
		"a zero clientSeq": r.c.TaskFacts(t.Context(), "3buqdlot", ok, 0),
		"no phase":         r.c.TaskFacts(t.Context(), "3buqdlot", envelope.TaskFacts{}, 1),
	} {
		if err == nil {
			t.Errorf("%s: sent", name)
		}
	}
	if len(r.b.auth) != 0 {
		t.Fatalf("%d requests reached the broker", len(r.b.auth))
	}
}

func TestLastSeqIsTheRoomsCurrentSeq(t *testing.T) {
	r := newRig(t, 7)
	if last, err := r.c.LastSeq(t.Context(), "3buqdlot"); err != nil || last != 7 {
		t.Fatalf("%d %v", last, err)
	}
	if len(r.b.limits) != 1 || r.b.limits[0] != "1" {
		t.Fatalf("one event read for its lastSeq: %v", r.b.limits)
	}
	if last, err := newRig(t, 0).c.LastSeq(t.Context(), "3buqdlot"); err != nil || last != 0 {
		t.Fatalf("an empty room is at 0: %d %v", last, err)
	}
	r.b.fail["/v1/rooms/3buqdlot/events"], r.b.reason = http.StatusNotFound, wire.ReasonNoRoom
	if _, err := r.c.LastSeq(t.Context(), "3buqdlot"); !errors.Is(err, ErrNoRoom) {
		t.Fatalf("the broker's refusal: %v", err)
	}
}
