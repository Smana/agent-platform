// SPDX-License-Identifier: Apache-2.0

// Command stub-model answers every chat completion with a call to the agent's finish tool, so
// quickstart-e2e proves the whole run path without a real model or its cost.
package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"
)

// The OpenHands SDK's FinishTool (openhands-sdk 1.49.6, tool/builtins/finish.py): tool name
// "finish", one required argument "message". Pinned by main_test.go.
const (
	finishTool = "finish"
	finishArgs = `{"message":"quickstart-e2e: nothing to change"}`
)

func toolCall() map[string]any {
	return map[string]any{"id": "call_1", "type": "function", "function": map[string]any{"name": finishTool, "arguments": finishArgs}}
}

func completion(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Stream bool `json:"stream"`
	}
	_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<20)).Decode(&req) // an unreadable body is a non-streaming ask
	created := time.Now().Unix()
	if req.Stream {
		w.Header().Set("Content-Type", "text/event-stream")
		call := toolCall()
		call["index"] = 0
		for _, c := range []map[string]any{
			{"index": 0, "delta": map[string]any{"role": "assistant", "content": "", "tool_calls": []any{call}}, "finish_reason": nil},
			{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"},
		} {
			b, _ := json.Marshal(map[string]any{"id": "stub", "object": "chat.completion.chunk", "created": created, "model": "stub", "choices": []any{c}})
			_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
		}
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": "stub", "object": "chat.completion", "created": created, "model": "stub",
		"choices": []any{map[string]any{"index": 0, "finish_reason": "tool_calls", "message": map[string]any{
			"role": "assistant", "content": "", "tool_calls": []any{toolCall()},
		}}},
		"usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
	})
}

func handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/chat/completions", completion)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	return mux
}

func main() {
	if err := (&http.Server{Addr: ":8080", Handler: handler(), ReadHeaderTimeout: 5 * time.Second}).ListenAndServe(); err != nil {
		slog.New(slog.NewTextHandler(os.Stderr, nil)).Error("stub-model", "err", err)
		os.Exit(1)
	}
}
