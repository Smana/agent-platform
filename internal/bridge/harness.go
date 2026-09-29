// SPDX-License-Identifier: Apache-2.0

// Package bridge is the room bridge inside the sandbox: the only client of the
// harness's local API besides agent-run, and the run's one pipe to the room.
package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/Smana/agent-platform/internal/httpx"
)

const (
	harnessTimeout = 10 * time.Second
	// pageLimit is the most events a page asks for. A page over maxResponseBytes
	// is asked again with half the limit, down to one event.
	pageLimit = 100
	// maxResponseBytes bounds one agent-server answer, and so the largest single
	// event the bridge can read.
	maxResponseBytes = 16 << 20
	// DefaultMaxPages is the MaxPages NewHarness sets. A Next call retains at
	// most MaxPages × maxResponseBytes of events and reads one more body on top
	// of that: 32 MiB at the default, inside the sidecar's 64 Mi limit.
	DefaultMaxPages = 1
)

var (
	// ErrEventTooLarge is one event alone over the response cap. The walk stops
	// before it and never moves past it: the next event's id sits in the same
	// unreadable body, and skipping blind would shift every later seq. The
	// bridge reports it (harness_error event_too_large) rather than drop it.
	ErrEventTooLarge = errors.New("a harness event is over the response cap")
	// ErrCursorLost is agent-server answering the cursor's page_id without that
	// event first. page_id is inclusive, so the server ignored it; accepting
	// the page would append events already in the log under new seqs.
	ErrCursorLost = errors.New("agent-server did not return the cursor's event first")
)

// RawEvent is one OpenHands event, kept whole for the mapping.
type RawEvent struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Source string `json:"source"`
	Raw    json.RawMessage
	// Malformed is an event without a non-empty string id, or with a kind or
	// source that is not a string. It is returned and counted, so the event
	// positions stay aligned, but it never keys the cursor.
	Malformed bool
}

// UnmarshalJSON reads the id, kind and source and keeps the whole event in Raw.
// It never fails: an event it cannot read is Malformed, so one bad event cannot
// fail its page and wedge every event after it.
func (e *RawEvent) UnmarshalJSON(b []byte) error {
	*e = RawEvent{Raw: append(json.RawMessage{}, b...)}
	var p struct{ ID, Kind, Source json.RawMessage }
	if json.Unmarshal(b, &p) != nil {
		e.Malformed = true
		return nil
	}
	idOK, kindOK, sourceOK := optString(p.ID, &e.ID), optString(p.Kind, &e.Kind), optString(p.Source, &e.Source)
	e.Malformed = !idOK || !kindOK || !sourceOK || e.ID == ""
	return nil
}

// optString reads an absent field as "" and a string as itself; any other JSON
// value is reported false.
func optString(v json.RawMessage, dst *string) bool {
	return len(v) == 0 || (v[0] == '"' && json.Unmarshal(v, dst) == nil)
}

// StatusError is agent-server answering with a non-2xx status. A 404 on Ready
// means agent-run has not created the conversation yet.
type StatusError struct {
	Method string
	Path   string
	Code   int
}

// Error names the request and the status, never a response body.
func (e *StatusError) Error() string {
	return fmt.Sprintf("agent-server %s %s: status %d", e.Method, e.Path, e.Code)
}

// Harness speaks agent-server 1.49.6 on loopback (ruling P4). It is not safe for
// concurrent Next and Skip calls on one cursor; the bridge owns its cursor.
type Harness struct {
	base string
	hc   *http.Client
	// MaxPages caps one Next call, so a backlog after a restart is read a few
	// pages per poll rather than all at once into a 64 Mi container (review M13).
	// NewHarness sets DefaultMaxPages; a value below one reads one page.
	MaxPages int
	maxBody  int64
}

// NewHarness returns the adapter for one conversation of the agent-server at base.
func NewHarness(base, conversationID string) *Harness {
	return &Harness{base: base + "/api/conversations/" + url.PathEscape(conversationID),
		hc: httpx.New(harnessTimeout, nil), MaxPages: DefaultMaxPages, maxBody: maxResponseBytes}
}

func (h *Harness) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("agent-server %s %s: %w", method, path, err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, h.base+path, body)
	if err != nil {
		return fmt.Errorf("agent-server %s %s: %w", method, path, err)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := h.hc.Do(req)
	if err != nil {
		return fmt.Errorf("agent-server %s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &StatusError{Method: method, Path: path, Code: resp.StatusCode}
	}
	b, err := httpx.ReadBody(resp.Body, h.maxBody)
	if err != nil {
		return fmt.Errorf("agent-server %s %s: %w", method, path, err)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("agent-server %s %s: %w", method, path, err)
	}
	return nil
}

// Ready succeeds once agent-run has created the conversation.
func (h *Harness) Ready(ctx context.Context) error {
	return h.do(ctx, http.MethodGet, "", nil, nil)
}

// Status is the conversation's execution_status (idle, running, paused,
// waiting_for_confirmation, finished, error or stuck).
func (h *Harness) Status(ctx context.Context) (string, error) {
	var info struct {
		ExecutionStatus string `json:"execution_status"`
	}
	err := h.do(ctx, http.MethodGet, "", nil, &info)
	return info.ExecutionStatus, err
}

// Page reads up to 100 events from pageID, inclusive, and the id of the first
// event of the next page ("" on the last page). A page over the response cap is
// asked again with half the limit; one event alone over it is ErrEventTooLarge.
func (h *Harness) Page(ctx context.Context, pageID string) ([]RawEvent, string, error) {
	for limit := pageLimit; ; limit /= 2 {
		evs, next, err := h.page(ctx, pageID, limit)
		switch {
		case !errors.Is(err, httpx.ErrBodyTooLarge):
			return evs, next, err
		case limit == 1:
			return nil, "", fmt.Errorf("%w: %w", ErrEventTooLarge, err)
		}
	}
}

func (h *Harness) page(ctx context.Context, pageID string, limit int) ([]RawEvent, string, error) {
	q := url.Values{"limit": {strconv.Itoa(limit)}, "sort_order": {"TIMESTAMP"}}
	if pageID != "" {
		q.Set("page_id", pageID)
	}
	var page struct {
		Items      []RawEvent      `json:"items"`
		NextPageID json.RawMessage `json:"next_page_id"`
	}
	if err := h.do(ctx, http.MethodGet, "/events/search?"+q.Encode(), nil, &page); err != nil {
		return nil, "", err
	}
	// An empty page that names a next one makes no progress, and a next id that
	// is not a string cannot be asked for: either ends the walk there. The next
	// poll starts again from the cursor.
	next := ""
	if len(page.Items) == 0 || !optString(page.NextPageID, &next) {
		next = ""
	}
	return page.Items, next, nil
}

// Cursor walks the append-only event log.
type Cursor struct {
	// LastID is the last well-formed event returned: the page the next walk
	// asks for. It is never a Malformed event's id, so it never rewinds.
	LastID string
	// Count is how many events were returned, Malformed ones included: the
	// event index SeqFor keys on.
	Count int64
	// Unkeyed is how many Malformed events after LastID were returned; the next
	// walk passes over them.
	Unkeyed int64
}

// Next returns the events after c, at most MaxPages pages of them. On error it
// returns what it read so far and the cursor advanced over exactly those.
func (h *Harness) Next(ctx context.Context, c Cursor) ([]RawEvent, Cursor, error) {
	var out []RawEvent
	c, err := h.walk(ctx, c, max(h.MaxPages, 1), func(Cursor) bool { return false },
		func(e RawEvent) { out = append(out, e) })
	return out, c, err
}

// Skip positions a fresh cursor after the first n events (a restarted bridge).
// With fewer than n events, the cursor stops after the last one. It retains
// nothing, so it reads as many pages as it takes.
func (h *Harness) Skip(ctx context.Context, n int64) (Cursor, error) {
	if n <= 0 {
		return Cursor{}, nil
	}
	return h.walk(ctx, Cursor{}, math.MaxInt, func(c Cursor) bool { return c.Count >= n }, func(RawEvent) {})
}

// walk reads the events after c page by page and hands each to take, until the
// log ends, maxPages pages returned events, or full holds. A page that returns
// nothing new (the cursor's own event and the ones it passes over, as a page
// halved to one event is) does not count: counting it would stall the cursor
// there forever. It stops on a page that cannot move it forward: an empty one,
// or one whose next page was already asked for or returned.
func (h *Harness) walk(ctx context.Context, c Cursor, maxPages int, full func(Cursor) bool,
	take func(RawEvent),
) (Cursor, error) {
	page, pass := c.LastID, c.Unkeyed
	asked, seen := map[string]bool{}, map[string]bool{c.LastID: true}
	for first, pages := true, 0; ; first = false {
		asked[page] = true
		evs, next, err := h.Page(ctx, page)
		if err != nil {
			return c, err
		}
		if first && c.LastID != "" {
			if len(evs) == 0 || evs[0].ID != c.LastID {
				return c, ErrCursorLost
			}
			evs = evs[1:] // page_id is inclusive
		}
		if int64(len(evs)) > pass {
			pages++
		}
		for _, e := range evs {
			if pass > 0 {
				pass-- // returned by an earlier walk
				continue
			}
			if full(c) {
				return c, nil
			}
			take(e)
			c.Count++
			if e.Malformed {
				c.Unkeyed++
				continue
			}
			c.LastID, c.Unkeyed = e.ID, 0
			seen[e.ID] = true
		}
		if next == "" || pages >= maxPages || full(c) || asked[next] || seen[next] {
			return c, nil
		}
		page = next
	}
}

// Send injects a message the agent consumes at its next step (steering, §2).
func (h *Harness) Send(ctx context.Context, text string) error {
	return h.do(ctx, http.MethodPost, "/events", map[string]any{"role": "user",
		"content": []map[string]string{{"type": "text", "text": text}}, "run": true}, nil)
}

// Respond answers the action the conversation is waiting to have confirmed.
func (h *Harness) Respond(ctx context.Context, accept bool, reason string) error {
	return h.do(ctx, http.MethodPost, "/events/respond_to_confirmation",
		map[string]any{"accept": accept, "reason": reason}, nil)
}

// Interrupt stops the agent's current step.
func (h *Harness) Interrupt(ctx context.Context) error {
	return h.do(ctx, http.MethodPost, "/interrupt", nil, nil)
}

// AlwaysConfirm makes every pending action wait for the bridge (§6; ruling P5).
func (h *Harness) AlwaysConfirm(ctx context.Context) error {
	return h.do(ctx, http.MethodPost, "/confirmation_policy",
		map[string]any{"policy": map[string]string{"kind": "AlwaysConfirm"}}, nil)
}
