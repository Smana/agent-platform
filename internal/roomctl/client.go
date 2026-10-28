// SPDX-License-Identifier: Apache-2.0

package roomctl

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
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/Smana/maku/internal/envelope"
	"github.com/Smana/maku/internal/httpx"
	"github.com/Smana/maku/internal/mcpwire"
	"github.com/Smana/maku/internal/roomview"
	"github.com/Smana/maku/internal/wire"
)

// Bounds on what the broker sends: the room list, and one frame (a state
// frame's queue and approvals, or an event of up to 64 KiB).
const (
	maxRoomsBody = 8 << 20
	maxFrame     = 4 << 20
	maxBackoff   = 30 * time.Second
)

// Client calls one broker as the developer, through oauth2-proxy, which lets
// the bearer through (skip-jwt-bearer-tokens).
type Client struct {
	URL     string                                // https://rooms.<private domain>
	Token   func(context.Context) (string, error) // a valid access token, asked for on every call
	HC      *http.Client                          // httpx: the room list
	Stream  *http.Client                          // httpx with no whole-exchange timeout: the WebSocket
	Backoff time.Duration                         // the first wait before a reconnect; zero is a second
	Issuer  string                                // the ZITADEL issuer, named in the unlinked hint
	Err     io.Writer                             // hints for the developer; nil is stderr
}

// RoomFilter narrows the room list; the broker applies it.
type RoomFilter struct {
	Repo          string // owner/name
	Mine, NeedsMe bool
	All           bool            // list the rooms that ended more than a week ago too; the overview filters, not the query
	Format        roomview.Format // how the overview is written; the zero value is Markdown, which is not for a terminal: callers set it
	JSON          bool            // print the broker's rows unchanged; not part of the query
}

func (f RoomFilter) query() string {
	q := url.Values{}
	if f.Repo != "" {
		q.Set("repo", f.Repo)
	}
	if f.Mine {
		q.Set("mine", "1")
	}
	if f.NeedsMe {
		q.Set("needs_me", "1")
	}
	return q.Encode()
}

// Refusal is the broker refusing a request before any room data: retrying
// cannot help, the developer has to act.
type Refusal struct {
	Op     string
	Code   int
	Reason string // the broker's reason, when it names one roomctl explains
}

// userInactive is the broker's 401 for a ZITADEL user no longer active (#126).
const userInactive = "user_inactive"

// refusal is resp as a Refusal, reading the reason from its body.
func refusal(op string, resp *http.Response) *Refusal {
	r := &Refusal{Op: op, Code: resp.StatusCode}
	if resp.Body != nil {
		body, _ := httpx.ReadBody(resp.Body, 1<<10)
		if strings.TrimSpace(string(body)) == userInactive {
			r.Reason = userInactive
		}
	}
	return r
}

func (r *Refusal) Error() string {
	why := http.StatusText(r.Code)
	switch {
	case r.Reason == userInactive:
		why = userInactive + ": your ZITADEL user is not active; an admin must reactivate it"
	case r.Code == http.StatusUnauthorized:
		why = "the token was refused: run roomctl login"
	case r.Code == http.StatusForbidden:
		why = "not in an agents group, or not allowed in this room"
	case r.Code == http.StatusNotFound:
		why = "no such room"
	}
	return fmt.Sprintf("%s: %d: %s", r.Op, r.Code, why)
}

// errToken is no access token to dial with: the token file is gone, or the
// issuer refused its refresh. Dialling again cannot help.
var errToken = errors.New("no access token")

func (c Client) header(ctx context.Context) (http.Header, error) {
	tok, err := c.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errToken, err)
	}
	h := http.Header{}
	if tok != "" { // a local broker takes no token, and an empty bearer would be a header it must refuse
		h.Set("Authorization", "Bearer "+tok)
	}
	return h, nil
}

// roomsBody is GET /api/rooms as the broker sent it, with its X-Rooms-Access (ruling R21).
func (c Client) roomsBody(ctx context.Context, f RoomFilter) (body []byte, access string, err error) {
	h, err := c.header(ctx)
	if err != nil {
		return nil, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL+"/api/rooms"+withQuery(f.query()), nil)
	if err != nil {
		return nil, "", err
	}
	req.Header = h
	resp, err := c.HC.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("rooms: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, "", refusal("rooms", resp)
	}
	body, err = httpx.ReadBody(resp.Body, maxRoomsBody)
	if err != nil {
		return nil, "", fmt.Errorf("rooms: %w", err)
	}
	return body, resp.Header.Get("X-Rooms-Access"), nil
}

// RoomRows is the rooms the developer may read, and X-Rooms-Access, which says why the list may be short.
func (c Client) RoomRows(ctx context.Context, f RoomFilter) ([]roomview.Row, string, error) {
	body, access, err := c.roomsBody(ctx, f)
	if err != nil {
		return nil, "", err
	}
	var rows []roomview.Row
	if err := json.Unmarshal(body, &rows); err != nil {
		return nil, "", fmt.Errorf("rooms: %w", err)
	}
	return rows, access, nil
}

// Rooms prints the overview, or with f.JSON the broker's rows as sent, then a hint on stderr
// when the broker says why the list may be short.
func (c Client) Rooms(ctx context.Context, out io.Writer, f RoomFilter) error {
	body, access, err := c.roomsBody(ctx, f)
	if err != nil {
		return err
	}
	var rows []roomview.Row
	if err := json.Unmarshal(body, &rows); err != nil { // before --json prints it: a garbled body is an error
		return fmt.Errorf("rooms: %w", err)
	}
	if f.JSON { // the broker's rows as sent: today's fields and the task's
		if _, err := fmt.Fprintln(out, string(body)); err != nil {
			return err
		}
		c.hint(access)
		return nil
	}
	if err := roomview.Overview(out, rows, roomview.Options{Format: f.Format, Now: time.Now(), All: f.All, Rooms: c.URL}); err != nil {
		return err
	}
	c.hint(access)
	return nil
}

func withQuery(q string) string {
	if q == "" {
		return ""
	}
	return "?" + q
}

// hint says why the list may be short. It never promises a retry: an
// unverified check can be a missing configuration.
func (c Client) hint(access string) {
	var msg string
	switch access {
	case "unlinked":
		msg = "hint: your ZITADEL account has no linked GitHub identity, so no room is listed: you see a room only if GitHub lets you read its repository. " +
			"Link it once: sign in at " + printable(c.Issuer) + ", choose GitHub and \"link\", then sign in with GitHub once."
	case "unverified":
		msg = "hint: room access could not be verified (the GitHub or ZITADEL check failed, or the broker has no access check configured), so this list may be incomplete."
	default:
		return
	}
	w := c.Err
	if w == nil {
		w = os.Stderr
	}
	_, _ = fmt.Fprintln(w, msg)
}

// Summary is GET /api/rooms/{id}/summary, the broker's summary/v1 body as sent.
func (c Client) Summary(ctx context.Context, room string, after int64) (json.RawMessage, error) {
	h, err := c.header(ctx)
	if err != nil {
		return nil, err
	}
	u := c.URL + "/api/rooms/" + url.PathEscape(room) + "/summary"
	if after > 0 {
		u += "?after=" + strconv.FormatInt(after, 10)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header = h
	resp, err := c.HC.Do(req)
	if err != nil {
		return nil, fmt.Errorf("status: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, errors.New("status: no such room (or you cannot read it)")
	case http.StatusServiceUnavailable:
		body, _ := httpx.ReadBody(resp.Body, 1<<10)
		if strings.Contains(string(body), "access_unverified") {
			return nil, errors.New("status: room access cannot be verified right now")
		}
		return nil, errors.New("status: room unavailable")
	default:
		return nil, refusal("status", resp)
	}
	body, err := httpx.ReadBody(resp.Body, maxRoomsBody)
	if err != nil {
		return nil, fmt.Errorf("status: %w", err)
	}
	return body, nil
}

// toolProtocol is the MCP revision a stateless tools/call names.
const toolProtocol = "2025-06-18"

// ErrUnknownTool is the broker answering -32602 to a tool it does not serve: an older broker.
var ErrUnknownTool = errors.New("the broker serves no such tool")

// Tool calls one tool of the broker's /mcp, statelessly (no initialize), as the developer. It
// returns the result's text and structuredContent. A broker with no /mcp is mcp_off, an isError
// result an error carrying its text, and -32602 an ErrUnknownTool.
func (c Client) Tool(ctx context.Context, name string, args any) (text string, structured json.RawMessage, err error) {
	raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args}})
	if err != nil {
		return "", nil, err
	}
	r := &Relay{URL: c.URL, Token: c.Token, HC: c.HC, version: toolProtocol}
	res, err := r.relay(ctx, raw)
	var rel relayed
	var refused *Refusal
	switch {
	case errors.As(err, &rel):
		var e struct {
			Code    int
			Message string
		}
		_ = json.Unmarshal(rel.e, &e)
		if e.Code == mcpwire.InvalidParams && strings.HasPrefix(strings.ToLower(e.Message), "unknown tool") {
			return "", nil, fmt.Errorf("%w: %s", ErrUnknownTool, OneLine(e.Message))
		}
		return "", nil, fmt.Errorf("%s: %s", name, OneLine(e.Message))
	case errors.Is(err, errToken), errors.As(err, &refused):
		return "", nil, errors.New(explainRelay(err))
	case err != nil:
		return "", nil, err
	}
	var out struct {
		Content []struct {
			Type, Text string
		}
		Structured json.RawMessage `json:"structuredContent"`
		IsError    bool            `json:"isError"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return "", nil, fmt.Errorf("%s: the broker's answer: %w", name, err)
	}
	var b strings.Builder
	for _, p := range out.Content {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	if out.IsError {
		return "", nil, fmt.Errorf("%s: %s", name, OneLine(b.String()))
	}
	return b.String(), out.Structured, nil
}

// dial opens the room's WebSocket and says hello.
func (c Client) dial(ctx context.Context, hello wire.ClientFrame) (*websocket.Conn, error) {
	h, err := c.header(ctx)
	if err != nil {
		return nil, err
	}
	conn, resp, err := websocket.Dial(ctx, c.URL+"/v1/ws?room="+url.QueryEscape(hello.RoomID),
		&websocket.DialOptions{HTTPClient: c.Stream, HTTPHeader: h})
	var refused *Refusal
	if err != nil && resp != nil && resp.StatusCode >= 400 && resp.StatusCode < 500 {
		refused = refusal("room "+hello.RoomID, resp)
	}
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		if refused != nil {
			return nil, refused
		}
		return nil, fmt.Errorf("room %s: %w", hello.RoomID, err)
	}
	conn.SetReadLimit(maxFrame)
	if err := wsjson.Write(ctx, conn, hello); err != nil {
		_ = conn.CloseNow()
		return nil, fmt.Errorf("room %s: hello: %w", hello.RoomID, err)
	}
	return conn, nil
}

// again reports whether the broker asked to be dialled again (docs/api.md: 4001
// reauth, 1001 shutdown, 1013 log_unavailable, 1008 slow_consumer), or the
// connection dropped without a close frame. A refusal, no token, a frame over
// the read limit or another close code (hello first) would fail the same way again.
func again(err error) bool {
	var refused *Refusal
	if errors.As(err, &refused) || errors.Is(err, errToken) || errors.Is(err, websocket.ErrMessageTooBig) {
		return false
	}
	switch websocket.CloseStatus(err) {
	case 4001, websocket.StatusGoingAway, websocket.StatusTryAgainLater, -1:
		return true
	case websocket.StatusPolicyViolation:
		var ce websocket.CloseError
		return errors.As(err, &ce) && ce.Reason == "slow_consumer"
	}
	return false
}

// Watch prints the room's state, its last tail events, then follows it until ctx ends.
func (c Client) Watch(ctx context.Context, room string, tail int, out io.Writer) error {
	return c.Follow(ctx, room, tail, out, nil)
}

// errFollowed ends a Follow whose stop accepted an event; Follow returns nil for it.
var errFollowed = errors.New("followed to the event asked for")

// CreateRoom is POST /api/rooms: a new room the developer owns and drives, on repository
// (owner/name) and dataClass (public or internal). It returns the room's id.
func (c Client) CreateRoom(ctx context.Context, repository, dataClass string) (string, error) {
	h, err := c.header(ctx)
	if err != nil {
		return "", err
	}
	body, err := json.Marshal(map[string]string{"repository": repository, "dataClass": dataClass})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+"/api/rooms", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header = h
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HC.Do(req)
	if err != nil {
		return "", fmt.Errorf("create room: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusCreated:
	case http.StatusUnauthorized, http.StatusForbidden:
		return "", refusal("create room", resp)
	default:
		msg, _ := httpx.ReadBody(resp.Body, 1<<10)
		return "", fmt.Errorf("create room: %d: %s", resp.StatusCode, OneLine(string(msg)))
	}
	raw, err := httpx.ReadBody(resp.Body, 1<<10)
	if err != nil {
		return "", fmt.Errorf("create room: %w", err)
	}
	var out struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(raw, &out) != nil || out.ID == "" {
		return "", errors.New("create room: the broker's answer has no room id")
	}
	return out.ID, nil
}

// Follow is Watch until stop, called on each event it prints, reports true: it returns nil then.
// A nil stop follows until ctx ends, which is not an error. Once connected, it dials again
// wherever the broker says to, with a fresh token, from the last seq it printed.
func (c Client) Follow(ctx context.Context, room string, tail int, out io.Writer, stop func(envelope.Event) bool) error {
	w := watch{c: c, out: out, stop: stop}
	first := c.Backoff
	if first <= 0 {
		first = time.Second
	}
	wait := first
	hello := wire.ClientFrame{Type: wire.FrameHello, RoomID: room, Tail: tail}
	for {
		states := w.states
		err := w.follow(ctx, hello)
		if w.states > states {
			wait = first // that connection got through: a later drop starts the backoff again
		}
		switch {
		case errors.Is(err, errFollowed):
			return nil
		case ctx.Err() != nil:
			return nil
		case !w.connected || !again(err): // a first dial that fails is a bad URL or room, not a blip
			return err
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(wait):
		}
		wait = min(wait*2, maxBackoff)
		last := w.last
		hello = wire.ClientFrame{Type: wire.FrameHello, RoomID: room, AfterSeq: &last}
	}
}

type watch struct {
	c         Client
	out       io.Writer
	stop      func(envelope.Event) bool
	connected bool
	states    int // state frames seen, one per connection that got through
	last      int64
}

func (w *watch) follow(ctx context.Context, hello wire.ClientFrame) error {
	conn, err := w.c.dial(ctx, hello)
	if err != nil {
		return err
	}
	defer func() { _ = conn.CloseNow() }()
	for {
		var f wire.ServerFrame
		if err := wsjson.Read(ctx, conn, &f); err != nil {
			if errors.Is(err, websocket.ErrMessageTooBig) {
				return fmt.Errorf("room %s: the broker sent a frame over %d MiB, which roomctl does not read: %w", hello.RoomID, maxFrame>>20, err)
			}
			return err
		}
		if f.Type == wire.FrameState {
			w.states++
		}
		switch {
		case f.Type == wire.FrameState && f.Snapshot != nil && !w.connected:
			w.connected = true
			s := f.Snapshot
			_, _ = fmt.Fprintln(w.out, printable(fmt.Sprintf("# room %s · %s · driver %s · you: %s", s.RoomID, s.Phase, s.Driver, s.You.Role)))
		case f.Type == wire.FrameEvent && f.Event != nil && f.Event.Seq > w.last:
			w.last = f.Event.Seq
			if _, err := fmt.Fprintln(w.out, Line(*f.Event)); err != nil {
				return err
			}
			if w.stop != nil && w.stop(*f.Event) {
				return errFollowed
			}
		}
	}
}

// Act sends one action and returns its ack, on a connection of its own that
// replays nothing: its hello asks from past the mark, which the broker clamps.
func (c Client) Act(ctx context.Context, room string, action map[string]any) (wire.ServerFrame, error) {
	after := int64(math.MaxInt64)
	conn, err := c.dial(ctx, wire.ClientFrame{Type: wire.FrameHello, RoomID: room, AfterSeq: &after})
	if err != nil {
		return wire.ServerFrame{}, err
	}
	defer func() { _ = conn.CloseNow() }()
	raw, err := json.Marshal(action)
	if err != nil {
		return wire.ServerFrame{}, err
	}
	if err := wsjson.Write(ctx, conn, wire.ClientFrame{Type: wire.FrameAct, ClientSeq: 1, Action: raw}); err != nil {
		return wire.ServerFrame{}, fmt.Errorf("room %s: act: %w", room, err)
	}
	for {
		var f wire.ServerFrame
		if err := wsjson.Read(ctx, conn, &f); err != nil {
			return wire.ServerFrame{}, fmt.Errorf("room %s: no ack: %w", room, err)
		}
		if f.Type == wire.FrameAck && f.ClientSeq == 1 {
			_ = conn.Close(websocket.StatusNormalClosure, "")
			return f, nil
		}
	}
}
