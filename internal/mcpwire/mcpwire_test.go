// SPDX-License-Identifier: Apache-2.0

package mcpwire

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecode(t *testing.T) {
	for name, c := range map[string]struct {
		body   string
		limit  int64
		status int
		code   int // 0: accepted
	}{
		"a request":               {`{"jsonrpc":"2.0","id":1,"method":"ping"}`, 1 << 10, http.StatusOK, 0},
		"a notification":          {`{"jsonrpc":"2.0","method":"notifications/initialized"}`, 1 << 10, http.StatusOK, 0},
		"a batch is no message":   {`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`, 1 << 10, http.StatusOK, ParseError},
		"trailing data":           {`{"jsonrpc":"2.0","id":1,"method":"ping"} {}`, 1 << 10, http.StatusOK, ParseError},
		"not JSON-RPC 2.0":        {`{"jsonrpc":"1.0","id":1,"method":"ping"}`, 1 << 10, http.StatusOK, InvalidRequest},
		"no method":               {`{"jsonrpc":"2.0","id":1}`, 1 << 10, http.StatusOK, InvalidRequest},
		"past the reader's limit": {`{"jsonrpc":"2.0","id":1,"method":"ping"}`, 16, http.StatusRequestEntityTooLarge, InvalidRequest},
	} {
		t.Run(name, func(t *testing.T) {
			_, status, err := Decode(http.MaxBytesReader(httptest.NewRecorder(), io.NopCloser(strings.NewReader(c.body)), c.limit))
			switch {
			case c.code == 0 && err != nil:
				t.Fatalf("refused: %+v", err)
			case c.code != 0 && (err == nil || err.Code != c.code || status != c.status):
				t.Fatalf("got %d %+v, want %d %d", status, err, c.status, c.code)
			}
		})
	}
}

func TestWriteAndToolResult(t *testing.T) {
	rec := httptest.NewRecorder()
	Write(rec, nil, nil, &Error{Code: MethodNotFound, Message: "no"})
	if got := strings.TrimSpace(rec.Body.String()); got != `{"error":{"code":-32601,"message":"no"},"id":null,"jsonrpc":"2.0"}` {
		t.Fatalf("%s", got)
	}
	res, err := ToolResult(map[string]string{"text": "a<b"})
	if err != nil {
		t.Fatal(err)
	}
	if text := res["content"].([]map[string]string)[0]["text"]; text != `{"text":"a<b"}` {
		t.Fatalf("the model's copy must read what was written: %s", text)
	}
	b, _ := json.Marshal(ToolError("no_room: no such room"))
	if string(b) != `{"content":[{"text":"no_room: no such room","type":"text"}],"isError":true}` {
		t.Fatalf("%s", b)
	}
}

func TestToolResultText(t *testing.T) {
	res, err := ToolResultText("**view**", map[string]int{"n": 1})
	if err != nil {
		t.Fatal(err)
	}
	c, _ := res["content"].([]map[string]string)
	if len(c) != 1 || c[0]["type"] != "text" || c[0]["text"] != "**view**" {
		t.Fatalf("content: %v", res["content"])
	}
	if s, _ := res["structuredContent"].(map[string]int); s["n"] != 1 {
		t.Fatalf("structuredContent: %v", res["structuredContent"])
	}
	if _, ok := res["isError"]; ok {
		t.Fatal("isError set")
	}
}
