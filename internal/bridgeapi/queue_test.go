// SPDX-License-Identifier: Apache-2.0

package bridgeapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/redact"
	"github.com/Smana/agent-platform/internal/store"
	"github.com/Smana/agent-platform/internal/wire"
)

// memQueue is the store's queue in memory: Enqueue checks what the store checks,
// SetQueued moves a row once, out of queued, and err fails every call;
// cursorErr fails Cursor alone.
type memQueue struct {
	drafts    []envelope.Draft
	rows      []store.Queued
	runs      map[int64]string
	err       error
	cursorErr error
}

func (q *memQueue) Enqueue(_ context.Context, d envelope.Draft, author, text string) (envelope.Event, error) {
	if q.err != nil {
		return envelope.Event{}, q.err
	}
	var p envelope.MessagePayload
	if err := d.Validate(); err != nil || json.Unmarshal(d.Payload, &p) != nil || p.Text != text ||
		p.Delivery != envelope.DeliveryQueued || author != d.Actor.ID {
		return envelope.Event{}, store.ErrNotAQueuedMessage
	}
	q.drafts = append(q.drafts, d)
	seq := int64(len(q.drafts))
	q.rows = append(q.rows, store.Queued{Ref: seq, Author: author, Text: text, State: "queued"})
	return envelope.Event{Seq: seq, RoomID: d.RoomID}, nil
}

func (q *memQueue) Queue(context.Context, string) ([]store.Queued, error) {
	var out []store.Queued
	for _, r := range q.rows {
		if r.State == "queued" {
			out = append(out, r)
		}
	}
	return out, q.err
}

func (q *memQueue) SetQueued(_ context.Context, _ string, ref int64, to, runID string) error {
	if q.err != nil {
		return q.err
	}
	if to != "consumed" || !envelope.ValidID(runID) {
		return store.ErrBadMove
	}
	for i := range q.rows {
		if q.rows[i].Ref == ref && q.rows[i].State == "queued" {
			q.rows[i].State, q.runs[ref] = to, runID
			return nil
		}
	}
	return store.ErrNotQueued
}

func (q *memQueue) Cursor(_ context.Context, _ string, origin string) (int64, error) {
	var hi int64
	for _, d := range q.drafts {
		if d.OriginClient == origin && d.OriginSeq > hi {
			hi = d.OriginSeq
		}
	}
	return hi, errors.Join(q.err, q.cursorErr)
}

func queueServer(t *testing.T) (http.Handler, *memQueue) {
	t.Helper()
	red, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}
	q := &memQueue{runs: map[int64]string{}}
	return (&Server{Systems: tokenAuth{"sys:"}, Redactor: red, Queue: q}).Routes(), q
}

// SP3 R9: a system caller queues a message in a room, redacted, lists the queue
// and consumes what a run's brief took.
func TestQueueRoutes(t *testing.T) {
	h, q := queueServer(t)
	const factory = "sys:system:factory"
	planted := "ghs_" + "Zq8mR2tXv9LkPw4NcYb7HsJ1fGdE6aUo3iTe" // pragma: allowlist secret (a repeated letter is too low-entropy to fire)
	msg := map[string]any{"text": "GitHub review by @Smana: use the relative link. token " + planted, "clientSeq": 901, "stream": "review"}
	if rec := call(t, h, "POST", "/v1/rooms/3buqdlot/queue", factory, msg); rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"seq":1`) {
		t.Fatalf("enqueue: %d %s", rec.Code, rec.Body)
	}
	if rec := call(t, h, "POST", "/v1/rooms/3buqdlot/queue", factory, msg); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"duplicate":true`) {
		t.Fatalf("a replay is a no-op: %d %s", rec.Code, rec.Body)
	}
	if len(q.drafts) != 1 || strings.Contains(q.rows[0].Text, planted) || !strings.Contains(q.rows[0].Text, "[REDACTED:") {
		t.Fatalf("one redacted, queued draft: %+v", q.rows)
	}
	d := q.drafts[0]
	if d.OriginClient != "system:factory:queue:review" || d.OriginSeq != 901 || d.Origin != envelope.OriginClient ||
		d.Actor != (envelope.Actor{Kind: envelope.ActorSystem, ID: "system:factory"}) || len(d.Redactions) == 0 {
		t.Fatalf("draft %+v", d)
	}
	var p envelope.MessagePayload
	if err := json.Unmarshal(d.Payload, &p); err != nil || p.Kind != envelope.KindChat || p.Delivery != envelope.DeliveryQueued || len(p.To) != 0 {
		t.Fatalf("payload %+v, %v", p, err)
	}

	// Each stream keeps its own clientSeq: 1 on another stream is new.
	other := map[string]any{"text": "CI failed on lint", "clientSeq": 1}
	if rec := call(t, h, "POST", "/v1/rooms/3buqdlot/queue", factory, other); rec.Code != http.StatusCreated || q.drafts[1].OriginClient != "system:factory:queue:default" {
		t.Fatalf("the default stream: %d %s", rec.Code, rec.Body)
	}

	rec := call(t, h, "GET", "/v1/rooms/3buqdlot/queue", factory, nil)
	var list struct {
		Queued []struct {
			Ref          int64
			Author, Text string
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); rec.Code != http.StatusOK || err != nil || len(list.Queued) != 2 ||
		list.Queued[0].Ref != 1 || list.Queued[0].Author != "system:factory" || list.Queued[0].Text != q.rows[0].Text {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}

	// A ref no longer queued, or never queued, is skipped, not an error.
	consume := map[string]any{"refs": []int64{1, 1, 99}, "runId": "aaaaaaaa"}
	if rec := call(t, h, "POST", "/v1/rooms/3buqdlot/queue/consume", factory, consume); rec.Code != http.StatusOK ||
		strings.TrimSpace(rec.Body.String()) != `{"consumed":1}` || q.runs[1] != "aaaaaaaa" {
		t.Fatalf("consume: %d %s %v", rec.Code, rec.Body, q.runs)
	}
	if rec := call(t, h, "GET", "/v1/rooms/3buqdlot/queue", factory, nil); strings.Contains(rec.Body.String(), `"ref":1,`) {
		t.Fatalf("a consumed message leaves the queue: %s", rec.Body)
	}
}

func TestQueueRoutesRefuse(t *testing.T) {
	h, q := queueServer(t)
	const factory = "sys:system:factory"
	msg := func(text string, seq int64, stream string) map[string]any {
		return map[string]any{"text": text, "clientSeq": seq, "stream": stream}
	}
	for _, c := range []struct {
		name, method, path, token string
		body                      any
		code                      int
		reason                    string
	}{
		{"no token", "POST", "/v1/rooms/3buqdlot/queue", "", msg("x", 1, ""), http.StatusUnauthorized, wire.ReasonUnauthenticated},
		{"a run token", "GET", "/v1/rooms/3buqdlot/queue", "run:aaaaaaaa", nil, http.StatusUnauthorized, wire.ReasonUnauthenticated},
		{"not allowlisted", "POST", "/v1/rooms/3buqdlot/queue/consume", "sys:forbidden", nil, http.StatusForbidden, wire.ReasonNotPermitted},
		{"a bad room", "GET", "/v1/rooms/NOT-C2/queue", factory, nil, http.StatusBadRequest, wire.ReasonBadRoom},
		{"no text", "POST", "/v1/rooms/3buqdlot/queue", factory, msg(" \n", 1, ""), http.StatusBadRequest, wire.ReasonBadMessage},
		{"no clientSeq", "POST", "/v1/rooms/3buqdlot/queue", factory, msg("x", 0, ""), http.StatusBadRequest, wire.ReasonBadMessage},
		{"an overlong text", "POST", "/v1/rooms/3buqdlot/queue", factory, msg(strings.Repeat("x", envelope.MaxHumanMessage+1), 1, ""), http.StatusBadRequest, wire.ReasonBadMessage},
		{"an unknown field", "POST", "/v1/rooms/3buqdlot/queue", factory, map[string]any{"text": "x", "clientSeq": 1, "author": "human:x"}, http.StatusBadRequest, wire.ReasonBadMessage},
		{"a stream with a colon", "POST", "/v1/rooms/3buqdlot/queue", factory, msg("x", 1, "a:b"), http.StatusBadRequest, wire.ReasonBadStream},
		{"an overlong stream", "POST", "/v1/rooms/3buqdlot/queue", factory, msg("x", 1, strings.Repeat("a", 17)), http.StatusBadRequest, wire.ReasonBadStream},
		{"a run id that is not C2", "POST", "/v1/rooms/3buqdlot/queue/consume", factory, map[string]any{"refs": []int64{1}, "runId": "NOPE"}, http.StatusBadRequest, wire.ReasonBadConsume},
		{"too many refs", "POST", "/v1/rooms/3buqdlot/queue/consume", factory, map[string]any{"refs": make([]int64, maxConsume+1), "runId": "aaaaaaaa"}, http.StatusBadRequest, wire.ReasonBadConsume},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec := call(t, h, c.method, c.path, c.token, c.body)
			if rec.Code != c.code || reason(t, rec) != c.reason {
				t.Fatalf("%d %s, want %d %s", rec.Code, rec.Body, c.code, c.reason)
			}
		})
	}
	if len(q.drafts) != 0 {
		t.Fatalf("a refusal queued %+v", q.drafts)
	}
	if rec := call(t, h, "POST", "/v1/rooms/3buqdlot/queue", factory, msg("x", 1, strings.Repeat("a", 16))); rec.Code != http.StatusCreated {
		t.Fatalf("a 16-letter stream: %d %s", rec.Code, rec.Body)
	}
}

// The store's sentinels map as the rest of the system API's do.
func TestQueueStoreFailures(t *testing.T) {
	h, q := queueServer(t)
	const factory = "sys:system:factory"
	msg := map[string]any{"text": "x", "clientSeq": 1}
	consume := map[string]any{"refs": []int64{1}, "runId": "aaaaaaaa"}
	for _, c := range []struct {
		err    error
		code   int
		reason string
	}{
		{store.ErrNoRoom, http.StatusNotFound, wire.ReasonNoRoom},
		{store.ErrSealed, http.StatusGone, wire.ReasonSealed},
		{errors.New("down"), http.StatusServiceUnavailable, wire.ReasonLogUnavailable},
	} {
		q.err = c.err
		for _, r := range []struct {
			method, path string
			body         any
		}{{"POST", "/v1/rooms/3buqdlot/queue", msg}, {"GET", "/v1/rooms/3buqdlot/queue", nil}, {"POST", "/v1/rooms/3buqdlot/queue/consume", consume}} {
			if rec := call(t, h, r.method, r.path, factory, r.body); rec.Code != c.code || reason(t, rec) != c.reason {
				t.Fatalf("%s %s on %v: %d %s", r.method, r.path, c.err, rec.Code, rec.Body)
			}
		}
	}
	// An unread cursor cannot tell a replay from a new message: nothing is queued.
	q.err, q.cursorErr = nil, errors.New("down")
	if rec := call(t, h, "POST", "/v1/rooms/3buqdlot/queue", factory, msg); rec.Code != http.StatusServiceUnavailable || len(q.drafts) != 0 {
		t.Fatalf("no cursor: %d %s, %d queued", rec.Code, rec.Body, len(q.drafts))
	}
}

func TestQueueRoutesWithoutAStore(t *testing.T) {
	red, err := redact.New()
	if err != nil {
		t.Fatal(err)
	}
	h := (&Server{Systems: tokenAuth{"sys:"}, Redactor: red}).Routes()
	for _, path := range []string{"/v1/rooms/3buqdlot/queue", "/v1/rooms/3buqdlot/queue/consume"} {
		if rec := call(t, h, "POST", path, "sys:system:factory", map[string]any{}); rec.Code != http.StatusNotImplemented || reason(t, rec) != wire.ReasonNoQueue {
			t.Fatalf("%s: %d %s", path, rec.Code, rec.Body)
		}
	}
	if rec := call(t, h, "GET", "/v1/rooms/3buqdlot/queue", "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("a caller is authenticated before anything else: %d", rec.Code)
	}
}
