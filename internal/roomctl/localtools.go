// SPDX-License-Identifier: Apache-2.0

package roomctl

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Smana/agent-platform/internal/mcpwire"
	"github.com/Smana/agent-platform/internal/roomview"
	"github.com/Smana/agent-platform/internal/summary"
)

// The local tools roomctl mcp serves beside the broker's, named once by mcpwire so the broker's
// instructions and this relay cannot drift (L16).
const (
	toolRoomWait     = mcpwire.RoomWait
	toolRoomCheckout = mcpwire.RoomCheckout
)

// localCall is one tools/call the relay serves itself.
type localCall struct {
	Name     string
	Args     json.RawMessage
	Progress json.RawMessage // the client's progressToken, empty when it asked for none
}

// localTool serves a call; its result is the tools/call result. It ends when ctx does: the
// client's cancellation, or the end of the session.
type localTool func(ctx context.Context, r *Relay, c localCall) any

// localHandlers are the relay's own tools, built once.
var localHandlers = buildHandlers()

func buildHandlers() map[string]localTool {
	unavailable := func(_ context.Context, _ *Relay, c localCall) any {
		return mcpwire.ToolError("unavailable: " + c.Name + " is not served by this roomctl yet")
	}
	return map[string]localTool{
		issueFileTool:    func(ctx context.Context, r *Relay, c localCall) any { return r.issueFile(ctx, c.Args) },
		toolRoomWait:     func(ctx context.Context, r *Relay, c localCall) any { return r.roomWait(ctx, c) },
		toolRoomCheckout: unavailable, // Task 17
	}
}

// localEntries are the local tools' tools/list entries, in the order they are appended.
var localEntries = []any{
	issueFileEntry,
	roomWaitEntry,
	map[string]any{
		"name":        toolRoomCheckout,
		"description": "Not yet available in this roomctl: check out the pull request a room opened into the developer's workspace.",
		"inputSchema": json.RawMessage(`{"type":"object","properties":{"room":{"type":"string"}}}`),
	},
}

// withLocalTools adds the local tools to the broker's tools/list result.
func withLocalTools(res json.RawMessage) (json.RawMessage, error) {
	var list map[string]any
	if err := json.Unmarshal(res, &list); err != nil {
		return nil, fmt.Errorf("tools/list: %w", err)
	}
	tools, _ := list["tools"].([]any)
	list["tools"] = append(tools, localEntries...)
	return json.Marshal(list)
}

// present is whether a capability object was sent: not absent, not null.
func present(raw json.RawMessage) bool {
	return len(raw) > 0 && string(bytes.TrimSpace(raw)) != "null"
}

// elicitationModes reads the client's elicitation capability: the empty object is form mode, as the
// spec says; otherwise each mode is the key that names it.
func elicitationModes(raw json.RawMessage) (forms, urls bool) {
	if !present(raw) {
		return false, false
	}
	var modes map[string]json.RawMessage
	if json.Unmarshal(raw, &modes) != nil {
		return false, false
	}
	_, forms = modes["form"]
	_, urls = modes["url"]
	return forms || (!urls), urls
}

// handler is the local tool called name.
func (r *Relay) handler(name string) (localTool, bool) {
	h := r.handlers
	if h == nil {
		h = localHandlers
	}
	t, ok := h[name]
	return t, ok
}

// localEntry is one local tool call in flight. It is compared by pointer: an id that comes back
// after a cancellation is another call.
type localEntry struct {
	call      localCall
	tool      localTool
	ctx       context.Context
	cancel    context.CancelFunc
	key       string
	cancelled bool // the client cancelled it: no reply
}

// callName is the tool a tools/call names.
func callName(params json.RawMessage) string {
	var c struct {
		Name string `json:"name"`
	}
	_ = json.Unmarshal(params, &c)
	return c.Name
}

// idKey is a request id as a map key: JSON numbers compare as numbers (7 is 7.0), and a string id
// is another id than the number that spells it.
func idKey(raw json.RawMessage) string {
	var v any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if dec.Decode(&v) != nil {
		return "raw:" + string(bytes.TrimSpace(raw))
	}
	switch id := v.(type) {
	case string:
		return "s:" + id
	case json.Number:
		if f, err := strconv.ParseFloat(string(id), 64); err == nil {
			return "n:" + strconv.FormatFloat(f, 'g', -1, 64)
		}
	}
	return "raw:" + string(bytes.TrimSpace(raw))
}

// reserve registers m when it is a call to a local tool, and returns its entry; nil otherwise.
func (r *Relay) reserve(ctx context.Context, m message) *localEntry {
	if m.Method != "tools/call" {
		return nil
	}
	var c struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
		Meta      struct {
			ProgressToken json.RawMessage `json:"progressToken"`
		} `json:"_meta"`
	}
	if json.Unmarshal(m.Params, &c) != nil {
		return nil
	}
	tool, ok := r.handler(c.Name)
	if !ok {
		return nil
	}
	e := &localEntry{tool: tool, key: idKey(m.ID), call: localCall{Name: c.Name, Args: c.Arguments, Progress: c.Meta.ProgressToken}}
	e.ctx, e.cancel = context.WithCancel(ctx)
	r.mu.Lock()
	r.local[e.key] = e
	r.mu.Unlock()
	return e
}

// release drops an entry whose call never started.
func (r *Relay) release(e *localEntry) {
	if e == nil {
		return
	}
	r.mu.Lock()
	if r.local[e.key] == e {
		delete(r.local, e.key)
	}
	r.mu.Unlock()
	e.cancel()
}

// callLocal runs a reserved local tool and answers m, unless the client cancelled it: a cancelled
// request is never answered.
func (r *Relay) callLocal(m message, e *localEntry) {
	res := e.tool(e.ctx, r, e.call)
	r.mu.Lock()
	live := !e.cancelled && e.ctx.Err() == nil // a session that ended is not written to
	if r.local[e.key] == e {
		delete(r.local, e.key)
	}
	r.mu.Unlock()
	e.cancel()
	if live {
		r.write(mcpwire.Response(m.ID, res, nil))
	}
}

// cancelLocal ends the local call a notifications/cancelled names, and says whether there was one.
// A call that is not the relay's is left for the broker.
func (r *Relay) cancelLocal(params json.RawMessage) bool {
	var p struct {
		RequestID json.RawMessage `json:"requestId"`
	}
	if json.Unmarshal(params, &p) != nil || len(p.RequestID) == 0 {
		return false
	}
	key := idKey(p.RequestID)
	r.mu.Lock()
	e, ok := r.local[key]
	if ok {
		e.cancelled = true
		delete(r.local, key)
	}
	r.mu.Unlock()
	if ok {
		e.cancel()
	}
	return ok
}

// callBroker calls one of the broker's tools as the developer: its text, its structuredContent,
// and whether it is a tool error. err is a failure to reach the broker, or its JSON-RPC error.
func (r *Relay) callBroker(ctx context.Context, tool string, args any) (text string, structured json.RawMessage, isError bool, err error) {
	r.mu.Lock()
	r.nextID++
	id := "roomctl-call-" + strconv.Itoa(r.nextID)
	r.mu.Unlock()
	raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args}})
	if err != nil {
		return "", nil, false, err
	}
	res, err := r.relay(ctx, raw)
	if err != nil {
		return "", nil, false, err
	}
	var out struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Structured json.RawMessage `json:"structuredContent"`
		IsError    bool            `json:"isError"`
	}
	if err := json.Unmarshal(res, &out); err != nil {
		return "", nil, false, fmt.Errorf("%s: %w", tool, err)
	}
	for _, c := range out.Content {
		if c.Type == "text" {
			text = c.Text
			break
		}
	}
	return text, out.Structured, out.IsError, nil
}

// progress tells the client how far a long call is, when it gave a progressToken.
func (r *Relay) progress(token json.RawMessage, done, total float64, msg string) {
	if len(token) == 0 || string(token) == "null" {
		return
	}
	p := map[string]any{"progressToken": token, "progress": done}
	if total > 0 {
		p["total"] = total
	}
	if msg != "" {
		p["message"] = msg
	}
	r.write(map[string]any{"jsonrpc": "2.0", "method": "notifications/progress", "params": p})
}

// workspace is where the developer works: the first root the client lists, else the process's
// working directory (a client without roots, one that does not answer in time, or a root that is
// not a file).
func (r *Relay) workspace(ctx context.Context) (string, error) {
	r.mu.Lock()
	roots := r.roots
	r.mu.Unlock()
	if roots {
		wait := r.RootsWait
		if wait <= 0 {
			wait = rootsWait
		}
		actx, cancel := context.WithTimeout(ctx, wait)
		ans, err := r.ask(actx, "roots/list", map[string]any{})
		cancel()
		if err == nil {
			var res struct {
				Roots []struct {
					URI string `json:"uri"`
				} `json:"roots"`
			}
			if json.Unmarshal(ans.Result, &res) == nil && len(res.Roots) > 0 {
				if u, err := url.Parse(res.Roots[0].URI); err == nil && u.Scheme == "file" && u.Path != "" {
					return u.Path, nil
				}
			}
		} else if ctx.Err() != nil {
			return "", ctx.Err()
		}
	}
	return os.Getwd()
}

// viewed gives rooms_list and room_summary results the Markdown view an older broker (FA14) did
// not write: when the text is JSON, it is replaced by the view of structuredContent, which stays
// as it was. Anything else passes through byte for byte.
func viewed(tool string, res json.RawMessage, rooms string, now time.Time) json.RawMessage {
	if tool != "rooms_list" && tool != "room_summary" {
		return res
	}
	var out map[string]json.RawMessage
	if json.Unmarshal(res, &out) != nil || string(out["isError"]) == "true" {
		return res
	}
	var content []json.RawMessage
	if json.Unmarshal(out["content"], &content) != nil || len(content) == 0 {
		return res
	}
	var first struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content[0], &first) != nil || first.Type != "text" {
		return res
	}
	if t := strings.TrimSpace(first.Text); t == "" || (t[0] != '{' && t[0] != '[') || !json.Valid([]byte(t)) {
		return res
	}
	o := roomview.Options{Format: roomview.Markdown, Now: now, Rooms: rooms}
	var view bytes.Buffer
	switch tool {
	case "rooms_list":
		var sc struct {
			Rooms *[]roomview.Row `json:"rooms"`
		}
		if json.Unmarshal(out["structuredContent"], &sc) != nil || sc.Rooms == nil || roomview.Overview(&view, *sc.Rooms, o) != nil {
			return res
		}
	default:
		var sum summary.Summary
		if json.Unmarshal(out["structuredContent"], &sum) != nil || sum.Room == "" || roomview.Card(&view, sum, o) != nil {
			return res
		}
	}
	head, err := json.Marshal(map[string]string{"type": "text", "text": view.String()})
	if err != nil {
		return res
	}
	if out["content"], err = json.Marshal(append([]json.RawMessage{head}, content[1:]...)); err != nil {
		return res
	}
	b, err := json.Marshal(out)
	if err != nil {
		return res
	}
	return b
}
