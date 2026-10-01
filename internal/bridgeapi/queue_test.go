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

// memQueue is the store's queue in memory, per room: Enqueue checks what the
// store checks and returns the stored event for a replayed key, SetQueued moves
// a row once, out of queued; err fails every call, cursorErr Cursor alone.
type memQueue struct {
	drafts    []envelope.Draft // every room's, in append order
	seqs      []int64          // drafts[i]'s seq in its room
	rows      map[string][]store.Queued
	runs      map[string]map[int64]string // the run that consumed each ref, by room
	err       error
	cursorErr error
}

func newMemQueue() *memQueue {
	return &memQueue{rows: map[string][]store.Queued{}, runs: map[string]map[int64]string{}}
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
	var seq int64
	for i, x := range q.drafts {
		if x.RoomID != d.RoomID {
			continue
		}
		if x.OriginClient == d.OriginClient && x.OriginSeq == d.OriginSeq {
			return envelope.Event{Seq: q.seqs[i], RoomID: d.RoomID}, nil
		}
		seq = q.seqs[i]
	}
	seq++
	q.drafts, q.seqs = append(q.drafts, d), append(q.seqs, seq)
	q.rows[d.RoomID] = append(q.rows[d.RoomID], store.Queued{Ref: seq, Author: author, Text: text, State: "queued"})
	return envelope.Event{Seq: seq, RoomID: d.RoomID}, nil
}

func (q *memQueue) Queue(_ context.Context, room string) ([]store.Queued, error) {
	var out []store.Queued
	for _, r := range q.rows[room] {
		if r.State == "queued" {
			out = append(out, r)
		}
	}
	return out, q.err
}

func (q *memQueue) SetQueued(_ context.Context, room string, ref int64, to, runID string) error {
	if q.err != nil {
		return q.err
	}
	if to != "consumed" || !envelope.ValidID(runID) {
		return store.ErrBadMove
	}
	rows := q.rows[room]
	for i := range rows {
		if rows[i].Ref == ref && rows[i].State == "queued" {
			rows[i].State = to
			if q.runs[room] == nil {
				q.runs[room] = map[int64]string{}
			}
			q.runs[room][ref] = runID
			return nil
		}
	}
	return store.ErrNotQueued
}

func (q *memQueue) Cursor(_ context.Context, room, origin string) (int64, error) {
	var hi int64
	for _, d := range q.drafts {
		if d.RoomID == room && d.OriginClient == origin && d.OriginSeq > hi {
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
	q := newMemQueue()
	return (&Server{Systems: tokenAuth{"sys:"}, Redactor: red, Queue: q}).Routes(), q
}

// SP3 R9: a system caller queues a message in a room, redacted, lists the queue
// and consumes what a run's brief took.
func TestQueueRoutes(t *testing.T) {
	h, q := queueServer(t)
	const factory = "sys:system:factory"
	const roomA, roomB = "/v1/rooms/3buqdlot/queue", "/v1/rooms/4kq7x2ma/queue"
	planted := "ghs_" + "Zq8mR2tXv9LkPw4NcYb7HsJ1fGdE6aUo3iTe" // pragma: allowlist secret (a repeated letter is too low-entropy to fire)
	review := func(seq int64, text string) map[string]any {
		return map[string]any{"text": text, "clientSeq": seq, "stream": "review"}
	}
	msg := review(901, "GitHub review by @Smana: use the relative link. token "+planted)
	if rec := call(t, h, "POST", roomA, factory, msg); rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"seq":1`) {
		t.Fatalf("enqueue: %d %s", rec.Code, rec.Body)
	}
	if rec := call(t, h, "POST", roomA, factory, msg); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"duplicate":true`) {
		t.Fatalf("a replay of the stream's latest is a no-op: %d %s", rec.Code, rec.Body)
	}
	rows := q.rows["3buqdlot"]
	if len(q.drafts) != 1 || strings.Contains(rows[0].Text, planted) || !strings.Contains(rows[0].Text, "[REDACTED:") {
		t.Fatalf("one redacted, queued draft: %+v", rows)
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

	// Review ids follow creation, not submission: a lower clientSeq is a new
	// message, and its replay is the store's to recognise (201, the same seq).
	for range 2 {
		if rec := call(t, h, "POST", roomA, factory, review(900, "created first, submitted second")); rec.Code != http.StatusCreated ||
			!strings.Contains(rec.Body.String(), `"seq":2`) || len(q.drafts) != 2 {
			t.Fatalf("an out-of-order clientSeq: %d %s, %d drafts", rec.Code, rec.Body, len(q.drafts))
		}
	}

	// Each stream keeps its own clientSeq: 1 on another stream is new.
	other := map[string]any{"text": "CI failed on lint", "clientSeq": 1}
	if rec := call(t, h, "POST", roomA, factory, other); rec.Code != http.StatusCreated || q.drafts[2].OriginClient != "system:factory:queue:default" {
		t.Fatalf("the default stream: %d %s", rec.Code, rec.Body)
	}

	rec := call(t, h, "GET", roomA, factory, nil)
	var list struct {
		Queued []struct {
			Ref          int64
			Author, Text string
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); rec.Code != http.StatusOK || err != nil || len(list.Queued) != 3 ||
		list.Queued[0].Ref != 1 || list.Queued[0].Author != "system:factory" || list.Queued[0].Text != rows[0].Text {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}

	// A ref no longer queued, or never queued, is skipped, not an error.
	consume := map[string]any{"refs": []int64{1, 1, 99}, "runId": "aaaaaaaa"}
	if rec := call(t, h, "POST", roomA+"/consume", factory, consume); rec.Code != http.StatusOK ||
		strings.TrimSpace(rec.Body.String()) != `{"consumed":1}` || q.runs["3buqdlot"][1] != "aaaaaaaa" {
		t.Fatalf("consume: %d %s %v", rec.Code, rec.Body, q.runs)
	}
	if rec := call(t, h, "GET", roomA, factory, nil); strings.Contains(rec.Body.String(), `"ref":1,`) {
		t.Fatalf("a consumed message leaves the queue: %s", rec.Body)
	}

	// Another room shares nothing: not the queue, not its refs, not the cursor.
	if rec := call(t, h, "GET", roomB, factory, nil); rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"queued":[]}` {
		t.Fatalf("another room's list: %d %s", rec.Code, rec.Body)
	}
	if rec := call(t, h, "POST", roomB+"/consume", factory, map[string]any{"refs": []int64{2, 3}, "runId": "aaaaaaaa"}); rec.Code != http.StatusOK ||
		strings.TrimSpace(rec.Body.String()) != `{"consumed":0}` || len(q.runs["3buqdlot"]) != 1 {
		t.Fatalf("refs queued in another room: %d %s %v", rec.Code, rec.Body, q.runs)
	}
	if rec := call(t, h, "POST", roomB, factory, msg); rec.Code != http.StatusCreated || !strings.Contains(rec.Body.String(), `"seq":1`) {
		t.Fatalf("the same clientSeq in another room: %d %s", rec.Code, rec.Body)
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
