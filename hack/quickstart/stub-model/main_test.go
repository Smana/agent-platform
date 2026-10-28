// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func post(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	handler().ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	return w
}

// The finish tool's name and argument key come from the pinned OpenHands SDK
// (openhands/sdk/tool/builtins/finish.py: FinishTool, FinishAction.message).
func TestCompletionCallsFinish(t *testing.T) {
	w := post(t, `{"model":"stub","messages":[]}`)
	var out struct {
		Choices []struct {
			FinishReason string `json:"finish_reason"`
			Message      struct {
				ToolCalls []struct {
					Function struct{ Name, Arguments string }
				} `json:"tool_calls"`
			}
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Choices) != 1 || out.Choices[0].FinishReason != "tool_calls" || len(out.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("response %s", w.Body)
	}
	f := out.Choices[0].Message.ToolCalls[0].Function
	var args map[string]string
	if f.Name != "finish" || json.Unmarshal([]byte(f.Arguments), &args) != nil || args["message"] == "" {
		t.Fatalf("function %+v", f)
	}
}

func TestStreamAnswersSSE(t *testing.T) {
	w := post(t, `{"stream":true}`)
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	b := w.Body.String()
	if !strings.Contains(b, `"name":"finish"`) || !strings.HasSuffix(b, "data: [DONE]\n\n") {
		t.Fatalf("stream %s", b)
	}
}

func TestHealthz(t *testing.T) {
	w := httptest.NewRecorder()
	handler().ServeHTTP(w, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil))
	if w.Code != http.StatusNoContent {
		t.Fatal(w.Code)
	}
}
