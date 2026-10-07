// SPDX-License-Identifier: Apache-2.0

package roomctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"text/tabwriter"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"

	"github.com/Smana/agent-platform/internal/httpx"
	"github.com/Smana/agent-platform/internal/wire"
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
}

// Refusal is the broker refusing a request before any room data: retrying
// cannot help, the developer has to act.
type Refusal struct {
	Op   string
	Code int
}

func (r *Refusal) Error() string {
	why := http.StatusText(r.Code)
	switch r.Code {
	case http.StatusUnauthorized:
		why = "the token was refused: run roomctl login"
	case http.StatusForbidden:
		why = "not in an agents group, or not allowed in this room"
	case http.StatusNotFound:
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
	h.Set("Authorization", "Bearer "+tok)
	return h, nil
}

// Rooms prints one row per room the developer may read.
func (c Client) Rooms(ctx context.Context, out io.Writer) error {
	h, err := c.header(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.URL+"/api/rooms", nil)
	if err != nil {
		return err
	}
	req.Header = h
	resp, err := c.HC.Do(req)
	if err != nil {
		return fmt.Errorf("rooms: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return &Refusal{Op: "rooms", Code: resp.StatusCode}
	}
	body, err := httpx.ReadBody(resp.Body, maxRoomsBody)
	if err != nil {
		return fmt.Errorf("rooms: %w", err)
	}
	var rows []struct {
		ID, Phase, Owner string
		LastSeq          int64
		You              struct{ Role string }
	}
	if err := json.Unmarshal(body, &rows); err != nil {
		return fmt.Errorf("rooms: %w", err)
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ROOM\tPHASE\tEVENTS\tOWNER\tYOU")
	for _, r := range rows {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n", printable(r.ID), printable(r.Phase), r.LastSeq, printable(r.Owner), printable(r.You.Role))
	}
	return tw.Flush()
}

// dial opens the room's WebSocket and says hello.
func (c Client) dial(ctx context.Context, hello wire.ClientFrame) (*websocket.Conn, error) {
	h, err := c.header(ctx)
	if err != nil {
		return nil, err
	}
	conn, resp, err := websocket.Dial(ctx, c.URL+"/v1/ws?room="+url.QueryEscape(hello.RoomID),
		&websocket.DialOptions{HTTPClient: c.Stream, HTTPHeader: h})
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		if resp != nil && resp.StatusCode >= 400 && resp.StatusCode < 500 {
			return nil, &Refusal{Op: "room " + hello.RoomID, Code: resp.StatusCode}
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

// Watch prints the room's state, its last tail events, then follows it until
// ctx ends, which is not an error. Once connected, it dials again wherever the
// broker says to, with a fresh token, from the last seq it printed.
func (c Client) Watch(ctx context.Context, room string, tail int, out io.Writer) error {
	w := watch{c: c, out: out}
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
