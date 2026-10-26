// SPDX-License-Identifier: Apache-2.0

// Package mcpwire is the wire both of the broker's MCP servers share: the agents' room tools on
// :8090 and the humans' /mcp on :8080. One JSON-RPC 2.0 message in, one answer out: no batch,
// nothing after the message. It knows no method and no caller; each server keeps its own. The
// names of the tools both sides must agree on are its only vocabulary.
package mcpwire

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// The tools the broker's /mcp and roomctl mcp share by name: roomctl mcp serves RoomWait and
// RoomCheckout itself, and the broker's instructions and prompts name them (one spelling, so the
// two cannot drift).
const (
	RoomWait     = "room_wait"
	RoomCheckout = "room_checkout"
	RoomChanges  = "room_changes"
)

// The JSON-RPC 2.0 error codes the servers answer with.
const (
	ParseError     = -32700
	InvalidRequest = -32600
	MethodNotFound = -32601
	InvalidParams  = -32602
	InternalError  = -32603
)

// Request is one JSON-RPC 2.0 request, or a notification when ID is empty.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// Error is a JSON-RPC error object. Data is left out when nil.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Decode reads exactly one JSON-RPC 2.0 message: no batch, nothing after it. A refusal comes with
// the HTTP status to send it under: 413 past the reader's MaxBytesReader, else 200.
func Decode(body io.Reader) (Request, int, *Error) {
	var req Request
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
		return req, http.StatusRequestEntityTooLarge, &Error{Code: InvalidRequest, Message: "request too large"}
	case err != nil:
		return req, http.StatusOK, &Error{Code: ParseError, Message: "parse error"}
	case req.JSONRPC != "2.0" || req.Method == "":
		return req, http.StatusOK, &Error{Code: InvalidRequest, Message: "not a JSON-RPC 2.0 request"}
	}
	return req, http.StatusOK, nil
}

// Response is the answer to id: result, or e when it is not nil. A nil id encodes as null.
func Response(id json.RawMessage, result any, e *Error) map[string]any {
	out := map[string]any{"jsonrpc": "2.0", "id": id}
	if e != nil {
		out["error"] = e
	} else {
		out["result"] = result
	}
	return out
}

// Write sends Response(id, result, e) as the JSON body. The caller writes any status first.
func Write(w http.ResponseWriter, id json.RawMessage, result any, e *Error) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(Response(id, result, e))
}

// ToolResult is a tools/call result carrying out twice: as structuredContent, and as the model's
// text copy, HTML left unescaped so it reads what was written.
func ToolResult(out any) (map[string]any, error) {
	var text bytes.Buffer
	enc := json.NewEncoder(&text)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(out); err != nil {
		return nil, err
	}
	return map[string]any{"content": []map[string]string{{"type": "text", "text": string(bytes.TrimSuffix(text.Bytes(), []byte("\n")))}},
		"structuredContent": out}, nil
}

// ToolResultText is a tools/call result whose text is a view for the human to read, and whose
// structuredContent is out, the data it was rendered from.
func ToolResultText(text string, out any) (map[string]any, error) {
	return map[string]any{"content": []map[string]string{{"type": "text", "text": text}}, "structuredContent": out}, nil
}

// DecodeArgs reads a tool's arguments, one JSON object, into dst: an unknown field or anything
// after the object is refused. No arguments, or null, is the empty object.
func DecodeArgs(args json.RawMessage, dst any) error {
	if len(bytes.TrimSpace(args)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data")
	}
	return nil
}

// ToolError is a tools/call result the model reads as a refusal: isError, and msg as its text.
func ToolError(msg string) map[string]any {
	return map[string]any{"isError": true, "content": []map[string]string{{"type": "text", "text": msg}}}
}
