// SPDX-License-Identifier: Apache-2.0

package rooms

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/httpx"
	"github.com/Smana/agent-platform/internal/wire"
)

const (
	// maxLimit is the broker's own cap on a page (bridgeapi maxRange): above it the broker
	// answers its default page of 100 without saying so, so the client refuses instead.
	maxLimit = 500
	// pageSize and maxPages bound EventsSince: the broker's default page, 10,000 events per call.
	pageSize = 100
	maxPages = 100
	// eventBytes bounds one event in a reply: a payload at the C4 cap plus its envelope.
	eventBytes = envelope.MaxPayload + 16<<10
	// maxReplyOverhead bounds the rest of a reply: lastSeq, an error, the JSON around the events.
	maxReplyOverhead = 4 << 10

	eventsRoute   = "/v1/rooms/{id}/events"
	messagesRoute = "/v1/rooms/{id}/messages"
)

// APIError is the broker's refusal: its status and wire reason (wire.Reason*), which callers
// branch on (no_room, sealed, rate_limited).
type APIError struct {
	Status int
	Reason string
}

func (e *APIError) Error() string { return fmt.Sprintf("broker: %d %s", e.Status, e.Reason) }

// The two refusals callers branch on, matched by errors.Is against an *APIError.
var (
	// ErrNoRoom is 404 no_room: the broker has not made the room's log yet. Retry later.
	ErrNoRoom = errors.New("rooms: the broker has no log for the room yet")
	// ErrNotPermitted is 403 not_permitted: the broker's systemPrincipals does not list the
	// factory. Expected until FR-1 enables the entry (SP2 M9); a config fix, not a retry.
	ErrNotPermitted = errors.New("rooms: the broker does not allow system:factory")
)

// Is matches ErrNoRoom and ErrNotPermitted.
func (e *APIError) Is(target error) bool {
	switch target {
	case ErrNoRoom:
		return e.Reason == wire.ReasonNoRoom
	case ErrNotPermitted:
		return e.Reason == wire.ReasonNotPermitted
	}
	return false
}

// Client calls the broker's system API as system:factory. The token is a projected
// ServiceAccount token (audience rooms-system) re-read before every call: kubelet rotates it.
type Client struct {
	base      *url.URL
	tokenFile string
	hc        *http.Client
	tracer    trace.Tracer
}

// New is the client for the broker at rawURL. It refuses anything but https://<host>[:port]
// (ruling SC): hc comes from httpx.New with the broker's CA (LoadCA). A nil tp traces nothing.
func New(rawURL, tokenFile string, hc *http.Client, tp trace.TracerProvider) (*Client, error) {
	u, err := url.Parse(rawURL)
	switch {
	case err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || strings.Trim(u.Path, "/") != "" ||
		u.RawQuery != "" || u.Fragment != "":
		return nil, errors.New("rooms: the broker URL must be https://<host>[:port], without credentials, path or query")
	case tokenFile == "":
		return nil, errors.New("rooms: a token file is required")
	case hc == nil:
		return nil, errors.New("rooms: an HTTP client from httpx.New is required")
	}
	if tp == nil {
		tp = noop.NewTracerProvider()
	}
	u.Path = ""
	return &Client{base: u, tokenFile: tokenFile, hc: hc, tracer: tp.Tracer("github.com/Smana/agent-platform/internal/factory/rooms")}, nil
}

// LoadCA reads the broker's CA bundle (broker.caFile, Secret openbao-ca) for httpx.New. It is read
// once at startup: the transport holds its pool, so a CA rotation needs a restart.
func LoadCA(path string) (*x509.CertPool, error) {
	raw, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("rooms: broker CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(raw) {
		return nil, fmt.Errorf("rooms: broker CA %s holds no PEM certificate", path)
	}
	return roots, nil
}

// Events reads one page of room after afterSeq: the events and the room's lastSeq. The reply is
// checked so that no caller can place a cursor wrong: every event is the room's, after afterSeq,
// in strictly increasing order, and there are at most limit of them.
func (c *Client) Events(ctx context.Context, room string, afterSeq int64, limit int) ([]envelope.Event, int64, error) {
	switch {
	case !envelope.ValidID(room):
		return nil, 0, fmt.Errorf("rooms: %q is not a C2 room id", room)
	case afterSeq < 0:
		return nil, 0, fmt.Errorf("rooms: afterSeq %d is negative", afterSeq)
	case limit < 1 || limit > maxLimit:
		return nil, 0, fmt.Errorf("rooms: limit %d is not 1..%d", limit, maxLimit)
	}
	var out struct {
		Events  []envelope.Event `json:"events"`
		LastSeq int64            `json:"lastSeq"`
	}
	q := url.Values{"afterSeq": {strconv.FormatInt(afterSeq, 10)}, "limit": {strconv.Itoa(limit)}}
	bound := int64(limit)*eventBytes + maxReplyOverhead
	if err := c.do(ctx, http.MethodGet, eventsRoute, room, "?"+q.Encode(), nil, bound, &out); err != nil {
		return nil, 0, err
	}
	if len(out.Events) > limit {
		return nil, 0, fmt.Errorf("rooms: the broker sent %d events for a limit of %d", len(out.Events), limit)
	}
	prev := afterSeq
	for _, e := range out.Events {
		if e.RoomID != room || e.Seq <= prev {
			return nil, 0, fmt.Errorf("rooms: the broker sent seq %d of room %q after seq %d of %s", e.Seq, e.RoomID, prev, room)
		}
		prev = e.Seq
	}
	return out.Events, out.LastSeq, nil
}

// EventsSince pages through room's log after afterSeq, pageSize at a time and at most maxPages
// pages. It returns the events read and the cursor to resume from: the seq of the last event read,
// or afterSeq when there was none. Never the room's lastSeq, which would skip what a capped read
// left behind.
func (c *Client) EventsSince(ctx context.Context, room string, afterSeq int64) ([]envelope.Event, int64, error) {
	var all []envelope.Event
	cursor := afterSeq
	for range maxPages {
		evs, _, err := c.Events(ctx, room, cursor, pageSize)
		if err != nil {
			return nil, 0, err
		}
		all = append(all, evs...)
		if len(evs) > 0 {
			cursor = evs[len(evs)-1].Seq
		}
		if len(evs) < pageSize {
			break
		}
	}
	return all, cursor, nil
}

// TaskState posts a task_state message (C4's reserved kind), system:* only. The broker keys a
// replay on (room, principal, clientSeq) alone: a new text under a seen clientSeq answers 200 and
// is not stored, so every distinct message needs its own clientSeq.
func (c *Client) TaskState(ctx context.Context, room, text string, clientSeq int64) error {
	switch {
	case !envelope.ValidID(room):
		return fmt.Errorf("rooms: %q is not a C2 room id", room)
	case clientSeq < 1:
		return fmt.Errorf("rooms: clientSeq %d is not positive", clientSeq)
	case len(text) > envelope.MaxHumanMessage:
		return fmt.Errorf("rooms: a task_state text is at most %d bytes", envelope.MaxHumanMessage)
	}
	in := struct {
		ClientSeq int64                `json:"clientSeq"`
		Kind      envelope.MessageKind `json:"kind"`
		Text      string               `json:"text"`
	}{clientSeq, envelope.KindTaskState, text}
	var out struct {
		Seq int64 `json:"seq"`
	}
	return c.do(ctx, http.MethodPost, messagesRoute, room, "", in, maxReplyOverhead, &out)
}

// spanReason bounds a refusal's reason, which the peer's reply body sets, to the system API's
// vocabulary: anything else is "other" on a span.
func spanReason(r string) string {
	switch r {
	case wire.ReasonBadRoom, wire.ReasonBadMessage, wire.ReasonUnauthenticated, wire.ReasonNotPermitted,
		wire.ReasonNoRoom, wire.ReasonSealed, wire.ReasonRateLimited, wire.ReasonLogUnavailable, wire.ReasonTimedOut:
		return r
	}
	return "other"
}

// do is one call. Its span carries metadata only (O-1 M3): the room id, the method, the route
// template, the status and the broker's reason, never a body, the query or the token.
func (c *Client) do(ctx context.Context, method, route, room, query string, in any, maxReply int64, out any) (err error) {
	ctx, span := c.tracer.Start(ctx, "broker "+method+" "+route, trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attribute.String("agent.room_id", room), attribute.String("http.request.method", method),
			attribute.String("url.template", route)))
	defer span.End()
	defer func() {
		if err == nil {
			return
		}
		why := "transport" // an error's text can quote a URL: only its class goes on the span
		var api *APIError
		if errors.As(err, &api) {
			why = spanReason(api.Reason)
		}
		span.SetAttributes(attribute.String("error.type", why))
		span.SetStatus(codes.Error, why)
	}()
	tok, err := os.ReadFile(filepath.Clean(c.tokenFile))
	if err != nil {
		return fmt.Errorf("rooms: token: %w", err)
	}
	bearer := strings.TrimSpace(string(tok))
	if bearer == "" {
		return errors.New("rooms: the token file is empty")
	}
	var body []byte
	if in != nil {
		if body, err = json.Marshal(in); err != nil {
			return fmt.Errorf("rooms: encode: %w", err)
		}
	}
	target := c.base.JoinPath(strings.Replace(route, "{id}", url.PathEscape(room), 1)).String() + query
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("rooms: request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	propagation.TraceContext{}.Inject(ctx, propagation.HeaderCarrier(req.Header))
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("rooms: %s %s: %w", method, route, err)
	}
	defer func() { _ = resp.Body.Close() }()
	status := resp.StatusCode
	span.SetAttributes(attribute.Int("http.response.status_code", status))
	raw, err := httpx.ReadBody(resp.Body, maxReply)
	if err != nil {
		return fmt.Errorf("rooms: %s %s: %w", method, route, err)
	}
	if status < 200 || status > 299 {
		var e wire.Error
		_ = json.Unmarshal(raw, &e)
		return &APIError{Status: status, Reason: e.Reason}
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("rooms: %s %s: the reply is not JSON", method, route)
	}
	return nil
}
