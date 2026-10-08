// SPDX-License-Identifier: Apache-2.0

// Package mcp is the room's MCP port, :8090 (§3). It is reachable only from the
// agent-router data plane (CNP), which authenticates the run first (MCPRoute
// oauth), injects X-Room-Mcp-Key, and sets X-Ar-Agent from the verified token. The
// run comes from that subject and its room and role from the AgentRun, never from
// a header or an argument. Tools only: Agent Router authorizes tools/call and
// tools/list and nothing else, so resources and prompts must not exist (§3). The
// server never sends a request of its own (agent-router#2715 drops
// server-to-client pings), so it answers POST only and opens no stream.
package mcp

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/policy"
	"github.com/Smana/agent-platform/internal/runwatch"
	"github.com/Smana/agent-platform/internal/store"
	"github.com/Smana/agent-platform/internal/version"
)

// The headers agent-router sets on every request it forwards: the key the
// MCPRoute injects (ruling P13) and the verified token's sub (C5).
const (
	KeyHeader      = "X-Room-Mcp-Key"
	IdentityHeader = "X-Ar-Agent"
)

const (
	// maxBody bounds a request. A 16 KiB text escaped by a client that writes
	// every '<' as < is 96 KiB of JSON, so the cap sits above that.
	maxBody = 128 << 10
	// callEvery is §4's room_* rate: one tool call a second per run.
	callEvery = time.Second
	// maxLimited bounds the limiter's memory. A run's last call over callEvery
	// ago is the same as none, so reaching the cap sweeps those.
	maxLimited = 4096
)

// protocols are the MCP revisions served; an unknown request gets the newest.
var protocols = []string{"2025-06-18", "2025-11-25", "2026-07-28"}

// Caller is the verified run a tool acts for.
type Caller struct{ Run runwatch.Run }

// Tool is one MCP tool. Action is the policy matrix's action the call needs
// (T7), and Roles the AgentRun roles that list and may call it. A tool with no
// Action is never callable: the matrix allows an agent nothing unnamed.
type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
	Action      policy.Action
	Roles       []string
	Call        func(ctx context.Context, c Caller, args json.RawMessage) (any, error)
}

// Runs is the liveness check on every request; *runwatch.Watcher implements it.
type Runs interface {
	Live(id string) (runwatch.Run, bool)
}

// Server is the :8090 handler. Key, Runs and SubPatterns (every run issuer's,
// each with one capture group: the run id) are required. OnReject counts a
// refusal by a bounded reason.
type Server struct {
	Key         func() string
	Runs        Runs
	SubPatterns []*regexp.Regexp
	Tools       []Tool
	OnReject    func(ctx context.Context, reason string)
	Logger      *slog.Logger
	Now         func() time.Time

	mu       sync.Mutex
	lastCall map[string]time.Time
}

// argError is a tool's refusal of its arguments, shown to the model as is.
type argError string

func (e argError) Error() string { return string(e) }

// rateError is a tool's own limit, shown to the model as is, beside the server's one call a second.
type rateError string

func (e rateError) Error() string { return string(e) }

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (s *Server) reject(ctx context.Context, reason string) {
	if s.OnReject != nil {
		s.OnReject(ctx, reason)
	}
}

func (s *Server) log() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.New(slog.DiscardHandler)
}

func (s *Server) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// allow spends runID's call for this second, if it has one.
func (s *Server) allow(runID string) bool {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.lastCall == nil {
		s.lastCall = map[string]time.Time{}
	}
	if last, ok := s.lastCall[runID]; ok && now.Sub(last) < callEvery {
		return false
	}
	if len(s.lastCall) >= maxLimited {
		for id, last := range s.lastCall {
			if now.Sub(last) >= callEvery {
				delete(s.lastCall, id)
			}
		}
	}
	if len(s.lastCall) >= maxLimited {
		return false // maxLimited runs each called within the second: fail closed
	}
	s.lastCall[runID] = now
	return true
}

// caller maps the router's headers to a live run, or answers the refusal.
func (s *Server) caller(w http.ResponseWriter, r *http.Request) (runwatch.Run, bool) {
	key, keys := s.Key(), r.Header.Values(KeyHeader)
	// An unset key must not admit a request that sends none.
	if key == "" || len(keys) != 1 || subtle.ConstantTimeCompare([]byte(keys[0]), []byte(key)) != 1 {
		s.reject(r.Context(), "mcp_key")
		w.WriteHeader(http.StatusUnauthorized)
		return runwatch.Run{}, false
	}
	// Exactly one subject: a second value, forged beside the gateway's, is
	// refused rather than guessed between (review M1).
	subjects := r.Header.Values(IdentityHeader)
	id := ""
	if len(subjects) == 1 {
		id = s.runID(subjects[0])
	}
	if id == "" {
		s.reject(r.Context(), "mcp_identity")
		w.WriteHeader(http.StatusForbidden)
		return runwatch.Run{}, false
	}
	run, ok := s.Runs.Live(id)
	if !ok || !envelope.ValidID(run.Room) {
		s.reject(r.Context(), "run_not_live")
		w.WriteHeader(http.StatusForbidden)
		return runwatch.Run{}, false
	}
	return run, true
}

// runID is the run the subject names under the first issuer pattern that
// matches it whole, or "". m[0] == the subject: a pattern missing its anchors
// must not accept a subject that merely contains a run's name (as authn.Runs).
func (s *Server) runID(subject string) string {
	for _, p := range s.SubPatterns {
		if m := p.FindStringSubmatch(subject); len(m) == 2 && m[0] == subject && envelope.ValidID(m[1]) {
			return m[1]
		}
	}
	return ""
}

// ServeHTTP answers one JSON-RPC request: initialize, ping, tools/list and
// tools/call. Every other method is -32601.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	run, ok := s.caller(w, r)
	if !ok {
		return
	}
	req, status, rerr := decode(http.MaxBytesReader(w, r.Body, maxBody))
	if rerr != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		reply(w, nil, nil, rerr)
		return
	}
	if len(req.ID) == 0 { // a notification: accepted, never answered
		w.WriteHeader(http.StatusAccepted)
		return
	}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		v := protocols[len(protocols)-1]
		if slices.Contains(protocols, p.ProtocolVersion) {
			v = p.ProtocolVersion
		}
		reply(w, req.ID, map[string]any{"protocolVersion": v,
			"capabilities": map[string]any{"tools": map[string]bool{"listChanged": false}},
			"serverInfo":   map[string]string{"name": "room-broker", "version": version.Version}}, nil)
	case "ping":
		reply(w, req.ID, map[string]any{}, nil)
	case "tools/list":
		tools := []map[string]any{}
		for _, t := range s.Tools {
			if slices.Contains(t.Roles, run.Role) {
				tools = append(tools, map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.InputSchema})
			}
		}
		reply(w, req.ID, map[string]any{"tools": tools}, nil)
	case "tools/call":
		s.call(r.Context(), w, req, run)
	default:
		reply(w, req.ID, nil, &rpcError{-32601, "the room MCP server exposes tools only"})
	}
}

// decode reads exactly one JSON-RPC 2.0 message: no batch, nothing after it.
func decode(body io.Reader) (request, int, *rpcError) {
	var req request
	dec := json.NewDecoder(body)
	err := dec.Decode(&req)
	if err == nil {
		if _, trailing := dec.Token(); !errors.Is(trailing, io.EOF) {
			err = errors.New("trailing data")
		}
	}
	var tooLarge *http.MaxBytesError
	switch {
	case errors.As(err, &tooLarge):
		return req, http.StatusRequestEntityTooLarge, &rpcError{-32600, "request too large"}
	case err != nil:
		return req, http.StatusOK, &rpcError{-32700, "parse error"}
	case req.JSONRPC != "2.0" || req.Method == "":
		return req, http.StatusOK, &rpcError{-32600, "not a JSON-RPC 2.0 request"}
	}
	return req, http.StatusOK, nil
}

func (s *Server) call(ctx context.Context, w http.ResponseWriter, req request, run runwatch.Run) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if len(req.Params) == 0 || json.Unmarshal(req.Params, &p) != nil || p.Name == "" {
		reply(w, req.ID, nil, &rpcError{-32602, "tools/call takes {name, arguments}"})
		return
	}
	// An unknown tool and one the role lacks answer alike.
	i := slices.IndexFunc(s.Tools, func(t Tool) bool { return t.Name == p.Name })
	agent := policy.Subject{Kind: envelope.ActorAgent, ID: "agent:" + run.ID}
	if i < 0 || !slices.Contains(s.Tools[i].Roles, run.Role) || !policy.Allowed(agent, s.Tools[i].Action) {
		s.reject(ctx, "tool_not_permitted")
		reply(w, req.ID, toolError("not_permitted: this tool is not one of the "+run.Role+" role's"), nil)
		return
	}
	if !s.allow(run.ID) {
		s.reject(ctx, "rate_limited")
		reply(w, req.ID, toolError("rate_limited: one room tool call per second"), nil)
		return
	}
	t := s.Tools[i]
	out, err := t.Call(ctx, Caller{Run: run}, p.Arguments)
	if err != nil {
		reply(w, req.ID, toolError(s.failure(ctx, t.Name, run, err)), nil)
		return
	}
	// The text is the model's copy: HTML left unescaped, so it reads what was written.
	var text bytes.Buffer
	enc := json.NewEncoder(&text)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		reply(w, req.ID, toolError(s.failure(ctx, t.Name, run, err)), nil)
		return
	}
	reply(w, req.ID, map[string]any{"content": []map[string]string{{"type": "text", "text": string(bytes.TrimSuffix(text.Bytes(), []byte("\n")))}},
		"structuredContent": out}, nil)
}

// failure maps a tool's error to what the model is told, in one place. Only an
// argument refusal is quoted: a database error's text can echo a value.
func (s *Server) failure(ctx context.Context, tool string, run runwatch.Run, err error) string {
	var bad argError
	var slow rateError
	switch {
	case errors.As(err, &bad):
		s.reject(ctx, "invalid_arguments")
		return "invalid_arguments: " + bad.Error()
	case errors.As(err, &slow):
		s.reject(ctx, "rate_limited")
		return "rate_limited: " + slow.Error()
	case errors.Is(err, store.ErrSealed):
		return "room_sealed: the room is closed to new events"
	case errors.Is(err, store.ErrNoRoom):
		return "no_room: the run's room has no log"
	}
	s.log().Error("room tool failed", "tool", tool, "room", run.Room, "run", run.ID, errAttr(err))
	return "log_unavailable: try again later"
}

// errAttr names a failure for the log. A database error's text can quote the
// value it refused, a payload, so only its SQLSTATE is logged.
func errAttr(err error) slog.Attr {
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		return slog.String("sqlstate", pg.Code)
	}
	return slog.String("err", err.Error())
}

func toolError(msg string) map[string]any {
	return map[string]any{"isError": true, "content": []map[string]string{{"type": "text", "text": msg}}}
}

func reply(w http.ResponseWriter, id json.RawMessage, result any, e *rpcError) {
	w.Header().Set("Content-Type", "application/json")
	out := map[string]any{"jsonrpc": "2.0", "id": id} // a nil id encodes as null
	if e != nil {
		out["error"] = e
	} else {
		out["result"] = result
	}
	_ = json.NewEncoder(w).Encode(out)
}
