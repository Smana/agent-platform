// SPDX-License-Identifier: Apache-2.0

package roomctl

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
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/Smana/agent-platform/internal/httpx"
	"github.com/Smana/agent-platform/internal/mcpwire"
)

// The relay's bounds.
const (
	maxLine     = 4 << 20 // one stdio message, either way
	maxReply    = 8 << 20 // one answer from the broker
	maxInFlight = 8       // requests relayed at once; past it, a request is refused, never queued
	defaultPoll = 5 * time.Second
	maxPoll     = 30 * time.Second
	roomWait    = 60 * time.Second // how long issue_file waits for the room of an issue it started
	rootsWait   = 5 * time.Second  // how long the relay waits for the client's roots
)

// elicitWait is how long issue_file waits for the developer's answer; a var so a test can shorten it.
var elicitWait = 10 * time.Minute

// Relay is roomctl mcp: an MCP server on stdio for a local coding agent. It relays each message to
// the broker's /mcp with the developer's token (factory interfaces F2), and serves one tool of its
// own, issue_file, which files a drafted issue with the developer's gh once they confirm it (F3).
// It blocks tasks/result itself, polling tasks/get, so no request to the broker outlives its proxy.
type Relay struct {
	URL   string                                // https://rooms.<private domain>
	Token func(context.Context) (string, error) // a valid access token, asked for on every request
	HC    *http.Client                          // httpx: each request bounded
	File  IssueFiler                            // files an accepted draft; nil refuses issue_file
	// Label puts a label on the issue just filed, as the developer; nil refuses to start one.
	Label Labeler
	// RoomWait bounds how long issue_file waits for the room of an issue it started; zero is roomWait.
	RoomWait time.Duration
	// After waits d; nil is time.After. Tests step their own.
	After func(d time.Duration) <-chan time.Time
	// Log is where roomctl mcp says what it did not do for the client (stderr in the CLI); nil is silent.
	Log *slog.Logger
	// RootsWait bounds the client's answer to roots/list; zero is rootsWait.
	RootsWait time.Duration
	// Git runs room_checkout's git; nil is RunGit. Tests replace it.
	Git Git
	// handlers are the local tools; nil is the relay's own (localHandlers). Tests replace them.
	handlers map[string]localTool

	out     io.Writer
	outMu   sync.Mutex
	mu      sync.Mutex
	version string                          // the revision initialize settled on
	forms   bool                            // the client can ask its human in a form (elicitation)
	urls    bool                            // the client can open a URL for its human (elicitation, URL mode)
	roots   bool                            // the client answers roots/list
	local   map[string]*localEntry          // the local tool calls in flight, by request id (idKey)
	waiting map[string]chan json.RawMessage // the relay's own requests to the client, by id
	nextID  int
}

// log is the relay's logger, discarding when none was injected.
func (r *Relay) log() *slog.Logger {
	if r.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return r.Log
}

// message is any JSON-RPC message, read far enough to route it.
type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   json.RawMessage `json:"error,omitempty"`
}

// Serve relays the messages read from in, one per line, writing each answer to out, until in ends
// or ctx does. Requests run concurrently, at most maxInFlight; the client's answers to the relay's
// own requests are read at once, so a request waiting on one never blocks the line.
func (r *Relay) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	r.out, r.waiting, r.local = out, map[string]chan json.RawMessage{}, map[string]*localEntry{}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	g := &errgroup.Group{}
	g.SetLimit(maxInFlight)
	// A room_wait sleeps for minutes: it has a pool of its own, so waits never take the slots of
	// the requests that answer at once, and no more than maxWaits are ever watching.
	gw := &errgroup.Group{}
	gw.SetLimit(maxWaits)
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64<<10), maxLine)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var m message
		if json.Unmarshal(line, &m) != nil || m.JSONRPC != "2.0" {
			r.write(mcpwire.Response(nil, nil, &mcpwire.Error{Code: mcpwire.ParseError, Message: "parse error"}))
			continue
		}
		if m.Method == "" { // an answer to one of the relay's requests
			r.answered(m)
			continue
		}
		raw := append([]byte(nil), line...)
		if len(m.ID) == 0 {
			// A notification is posted before the next line is read: MCP wants the client's
			// notifications (initialized above all) to reach the broker ahead of its later requests.
			// A hung broker holds the line only as long as one forward, which HC bounds.
			r.handle(ctx, m, raw, nil)
			continue
		}
		// A local call is registered here, before its goroutine starts: the client's cancellation
		// is handled inline, and may be the very next line.
		pre := r.reserve(ctx, m)
		pool, busy := g, "busy: 8 requests in flight"
		if pre != nil && pre.call.Name == toolRoomWait {
			pool, busy = gw, "busy: 8 room_wait calls in flight"
		}
		if !pool.TryGo(func() error { r.handle(ctx, m, raw, pre); return nil }) {
			r.release(pre)
			r.write(mcpwire.Response(m.ID, nil, &mcpwire.Error{Code: mcpwire.InternalError, Message: busy}))
		}
	}
	cancel() // stdin closed: the client is gone, so are the answers it waited for
	_ = g.Wait()
	_ = gw.Wait()
	return sc.Err()
}

// handle serves one request or notification.
func (r *Relay) handle(ctx context.Context, m message, raw []byte, pre *localEntry) {
	if len(m.ID) == 0 { // a notification: relayed, never answered
		if m.Method == "notifications/cancelled" && r.cancelLocal(m.Params) {
			return // the client's cancellation of a call only the relay serves
		}
		_, _ = r.forward(ctx, raw)
		return
	}
	switch m.Method {
	case "initialize":
		var p struct {
			Capabilities struct {
				Elicitation json.RawMessage `json:"elicitation"`
				Roots       json.RawMessage `json:"roots"`
			} `json:"capabilities"`
		}
		_ = json.Unmarshal(m.Params, &p)
		forms, urls := elicitationModes(p.Capabilities.Elicitation)
		r.mu.Lock()
		r.forms, r.urls = forms, urls
		r.roots = present(p.Capabilities.Roots)
		r.mu.Unlock()
		res, err := r.relay(ctx, raw)
		if err == nil {
			var v struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			if json.Unmarshal(res, &v) == nil {
				r.mu.Lock()
				r.version = v.ProtocolVersion
				r.mu.Unlock()
			}
		}
		r.reply(m, res, err)
	case "tools/list":
		res, err := r.relay(ctx, raw)
		if err == nil {
			res, err = withLocalTools(res)
		}
		r.reply(m, res, err)
	case "tools/call":
		if pre != nil {
			r.callLocal(m, pre)
			return
		}
		if roomRefTools[callName(m.Params)] {
			var refusal any
			if raw, refusal = r.resolveCall(ctx, m); refusal != nil {
				r.write(mcpwire.Response(m.ID, refusal, nil))
				return
			}
		}
		res, err := r.relay(ctx, raw)
		if err == nil {
			res = viewed(callName(m.Params), res, r.URL, time.Now())
		}
		r.reply(m, res, err)
	case "tasks/result":
		res, err := r.taskResult(ctx, m, raw)
		r.reply(m, res, err)
	default:
		res, err := r.relay(ctx, raw)
		r.reply(m, res, err)
	}
}

// relayed is the broker's JSON-RPC answer to one request: its result, or its error as is.
type relayed struct{ e json.RawMessage }

func (e relayed) Error() string { return string(e.e) }

// relay sends raw to the broker and returns the answer's result; a JSON-RPC error comes back as
// relayed, a refusal of the request as a *Refusal or a transport error.
func (r *Relay) relay(ctx context.Context, raw []byte) (json.RawMessage, error) {
	body, err := r.forward(ctx, raw)
	if err != nil {
		return nil, err
	}
	var a message
	if err := json.Unmarshal(body, &a); err != nil {
		return nil, fmt.Errorf("the broker's answer: %w", err)
	}
	if len(a.Error) > 0 {
		return nil, relayed{a.Error}
	}
	return a.Result, nil
}

// reply answers m with res, or with err: the broker's own error as is; a refusal or a failure to
// reach it as a tool error for tools/call, so the model can tell the developer, else as -32603.
func (r *Relay) reply(m message, res json.RawMessage, err error) {
	var rel relayed
	switch {
	case err == nil:
		r.write(mcpwire.Response(m.ID, res, nil))
	case errors.As(err, &rel):
		r.write(map[string]any{"jsonrpc": "2.0", "id": m.ID, "error": rel.e})
	case m.Method == "tools/call":
		r.write(mcpwire.Response(m.ID, mcpwire.ToolError(explainRelay(err)), nil))
	default:
		r.write(mcpwire.Response(m.ID, nil, &mcpwire.Error{Code: mcpwire.InternalError, Message: explainRelay(err)}))
	}
}

// explainRelay says what the developer does about err.
func explainRelay(err error) string {
	var refused *Refusal
	switch {
	case errors.Is(err, errToken), errors.As(err, &refused) && refused.Code == http.StatusUnauthorized && refused.Reason == "":
		return "not_logged_in: run roomctl login in a terminal; roomctl mcp reads the new token on the next call, with no restart"
	case errors.As(err, &refused) && refused.Code == http.StatusNotFound:
		return "mcp_off: the broker serves no /mcp; an operator turns on human.mcp.enabled"
	case errors.As(err, &refused) && refused.Reason == mcpNeedsRoomctl:
		return mcpNeedsRoomctl + ": the broker takes roomctl's own token only"
	case errors.As(err, &refused):
		return refused.Error()
	}
	return "unavailable: " + err.Error()
}

// mcpNeedsRoomctl is the broker's 403 for a token that is not roomctl's.
const mcpNeedsRoomctl = "mcp_needs_roomctl"

// forward posts raw to the broker's /mcp and returns the JSON-RPC answer: nil for a notification.
func (r *Relay) forward(ctx context.Context, raw []byte) ([]byte, error) {
	tok, err := r.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errToken, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.URL+"/mcp", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	r.mu.Lock()
	if r.version != "" {
		req.Header.Set("MCP-Protocol-Version", r.version)
	}
	r.mu.Unlock()
	resp, err := r.HC.Do(req)
	if err != nil {
		return nil, fmt.Errorf("mcp: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusAccepted:
		return nil, nil
	case http.StatusOK, http.StatusBadRequest: // a 400 carries a JSON-RPC error: the protocol version
		if resp.StatusCode == http.StatusOK || strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
			body, err := httpx.ReadBody(resp.Body, maxReply)
			if err != nil {
				return nil, fmt.Errorf("mcp: %w", err)
			}
			if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
				return lastData(body), nil
			}
			return body, nil
		}
	}
	why, _ := httpx.ReadBody(resp.Body, 1<<10)
	ref := &Refusal{Op: "mcp", Code: resp.StatusCode}
	if r := strings.TrimSpace(string(why)); r == userInactive || r == mcpNeedsRoomctl {
		ref.Reason = r
	}
	return nil, ref
}

// lastData is a server-sent event stream's last data line: the answer.
func lastData(body []byte) []byte {
	var last []byte
	for line := range bytes.SplitSeq(body, []byte("\n")) {
		if d, ok := bytes.CutPrefix(bytes.TrimRight(line, "\r"), []byte("data: ")); ok {
			last = d
		}
	}
	return last
}

// taskResult blocks for the client, which may: it polls tasks/get at the task's pollInterval
// until the task is terminal, then relays tasks/result.
func (r *Relay) taskResult(ctx context.Context, m message, raw []byte) (json.RawMessage, error) {
	var p struct {
		TaskID string `json:"taskId"`
	}
	_ = json.Unmarshal(m.Params, &p)
	get, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "roomctl-poll", "method": "tasks/get", "params": map[string]string{"taskId": p.TaskID}})
	for {
		res, err := r.relay(ctx, get)
		if err != nil {
			return nil, err
		}
		var t struct {
			Status       string `json:"status"`
			PollInterval int64  `json:"pollInterval"`
		}
		if err := json.Unmarshal(res, &t); err != nil {
			return nil, fmt.Errorf("tasks/get: %w", err)
		}
		if t.Status != "working" && t.Status != "input_required" {
			return r.relay(ctx, raw)
		}
		wait := min(max(time.Duration(t.PollInterval)*time.Millisecond, time.Second), maxPoll)
		if t.PollInterval <= 0 {
			wait = defaultPoll
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-r.after(wait):
		}
	}
}

func (r *Relay) after(d time.Duration) <-chan time.Time {
	if r.After != nil {
		return r.After(d)
	}
	return time.After(d)
}

// ask sends the client a request of the relay's own and waits for its answer, its result or its
// error object, for at most elicitWait.
func (r *Relay) ask(ctx context.Context, method string, params any) (message, error) {
	r.mu.Lock()
	r.nextID++
	id := "roomctl-" + strconv.Itoa(r.nextID)
	ch := make(chan json.RawMessage, 1)
	r.waiting[id] = ch
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.waiting, id)
		r.mu.Unlock()
	}()
	r.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	ctx, cancel := context.WithTimeout(ctx, elicitWait)
	defer cancel()
	select {
	case <-ctx.Done():
		// Take the question back: the client stops asking its human, and its answer is not waited for.
		r.write(map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled",
			"params": map[string]any{"requestId": id, "reason": "the request that needed the answer ended"}})
		return message{}, ctx.Err()
	case raw := <-ch:
		var m message
		return m, json.Unmarshal(raw, &m)
	}
}

// answered hands the client's answer to the request of the relay's that waits for it.
func (r *Relay) answered(m message) {
	var id string
	if json.Unmarshal(m.ID, &id) != nil {
		return
	}
	r.mu.Lock()
	ch, ok := r.waiting[id]
	r.mu.Unlock()
	if ok {
		raw, _ := json.Marshal(m)
		ch <- raw
	}
}

// write sends one message to the client, a line at a time.
func (r *Relay) write(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	r.outMu.Lock()
	defer r.outMu.Unlock()
	_, _ = r.out.Write(append(b, '\n'))
}

// resolveCall is the tools/call m with its room reference resolved to an id, or the refusal.
func (r *Relay) resolveCall(ctx context.Context, m message) ([]byte, any) {
	var p map[string]json.RawMessage
	if json.Unmarshal(m.Params, &p) != nil {
		return nil, nil
	}
	args, refusal := r.resolveArgs(ctx, p["arguments"], "room")
	if refusal != nil {
		return nil, refusal
	}
	if args != nil {
		p["arguments"] = args
	}
	params, _ := json.Marshal(p)
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.ID, "method": m.Method, "params": json.RawMessage(params)})
	return raw, nil
}
