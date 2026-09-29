// SPDX-License-Identifier: Apache-2.0

// Package bridge is the room bridge inside the sandbox: the only client of the
// harness's local API besides agent-run, and the run's one pipe to the room.
package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/Smana/agent-platform/internal/httpx"
)

const (
	harnessTimeout = 10 * time.Second
	pageLimit      = "100"
	// maxResponseBytes bounds one agent-server answer, a page of 100 events at
	// most, inside the sidecar's 64 Mi limit.
	maxResponseBytes = 16 << 20
)

// RawEvent is one OpenHands event, kept whole for the mapping.
type RawEvent struct {
	ID     string `json:"id"`
	Kind   string `json:"kind"`
	Source string `json:"source"`
	Raw    json.RawMessage
}

// UnmarshalJSON reads the id, kind and source and keeps the whole event in Raw.
func (e *RawEvent) UnmarshalJSON(b []byte) error {
	var p struct{ ID, Kind, Source string }
	if err := json.Unmarshal(b, &p); err != nil {
		return fmt.Errorf("harness event: %w", err)
	}
	e.ID, e.Kind, e.Source, e.Raw = p.ID, p.Kind, p.Source, append(json.RawMessage{}, b...)
	return nil
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
	// MaxPages caps one Next call (0: no cap), so a backlog after a restart is read a
	// few pages per poll rather than all at once into a 64 Mi container (review M13).
	MaxPages int
}

// NewHarness returns the adapter for one conversation of the agent-server at base.
func NewHarness(base, conversationID string) *Harness {
	return &Harness{base: base + "/api/conversations/" + url.PathEscape(conversationID),
		hc: httpx.New(harnessTimeout, nil)}
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
	b, err := httpx.ReadBody(resp.Body, maxResponseBytes)
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
// event of the next page ("" on the last page).
func (h *Harness) Page(ctx context.Context, pageID string) ([]RawEvent, string, error) {
	q := url.Values{"limit": {pageLimit}, "sort_order": {"TIMESTAMP"}}
	if pageID != "" {
		q.Set("page_id", pageID)
	}
	var page struct {
		Items      []RawEvent `json:"items"`
		NextPageID *string    `json:"next_page_id"`
	}
	if err := h.do(ctx, http.MethodGet, "/events/search?"+q.Encode(), nil, &page); err != nil {
		return nil, "", err
	}
	next := ""
	if page.NextPageID != nil && len(page.Items) > 0 {
		// An empty page that names a next one makes no progress: end the walk
		// there rather than loop on it.
		next = *page.NextPageID
	}
	return page.Items, next, nil
}

// Cursor walks the append-only event log: the last event seen, and how many.
type Cursor struct {
	LastID string
	Count  int64
}

// Next returns the events after c. On error it returns what it read so far and
// the cursor advanced over exactly those.
func (h *Harness) Next(ctx context.Context, c Cursor) ([]RawEvent, Cursor, error) {
	var out []RawEvent
	page, skipFirst := c.LastID, c.LastID != ""
	for pages := 1; ; pages++ {
		evs, next, err := h.Page(ctx, page)
		if err != nil {
			return out, c, err
		}
		for i, e := range evs {
			if skipFirst && i == 0 && e.ID == c.LastID {
				continue // page_id is inclusive
			}
			out = append(out, e)
			c.LastID, c.Count = e.ID, c.Count+1
		}
		skipFirst = false
		if next == "" || (h.MaxPages > 0 && pages >= h.MaxPages) {
			return out, c, nil
		}
		page = next
	}
}

// Skip positions a fresh cursor after the first n events (a restarted bridge).
// With fewer than n events, the cursor stops after the last one.
func (h *Harness) Skip(ctx context.Context, n int64) (Cursor, error) {
	var c Cursor
	page := ""
	for c.Count < n {
		evs, next, err := h.Page(ctx, page)
		if err != nil {
			return c, err
		}
		for _, e := range evs {
			if c.Count == n {
				break
			}
			c.LastID, c.Count = e.ID, c.Count+1
		}
		if next == "" {
			break
		}
		page = next
	}
	return c, nil
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
