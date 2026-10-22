// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/fanout"
	"github.com/Smana/agent-platform/internal/roomctl"
	"github.com/Smana/agent-platform/internal/store"
)

// bearerAuth is headerAuth for roomctl's bearer: "Bearer <user>" is <user> through roomctl's client.
type bearerAuth struct{}

func (bearerAuth) Authenticate(r *http.Request) (authn.Principal, error) {
	who, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	c := r.Clone(r.Context())
	c.Header = header(who, "X-Test-Client", "roomctl")
	return headerAuth{}.Authenticate(c)
}

// The design's live proof 1, offline: an agent on roomctl mcp lists, reads, queues (wrapped),
// tails as a task, and files a draft only once its developer accepts it.
func TestRoomctlMCPAgainstTheBroker(t *testing.T) {
	l := &actLog{st: store.RoomState{ID: roomID, Driver: "system:factory", DriverEpoch: 3}}
	e := setup(t, mcpOn, actorOn(l), func(s *Server, _ *fanout.Hub, _ *hubView) { s.Humans = bearerAuth{} })
	var mu sync.Mutex
	var filed []string
	polled := make(chan struct{}, 8)
	relay := &roomctl.Relay{URL: e.ts.URL, HC: e.ts.Client(),
		Token: func(context.Context) (string, error) { return "own", nil },
		File: func(_ context.Context, repo, title, body string) (string, error) {
			mu.Lock()
			defer mu.Unlock()
			filed = append(filed, repo+"|"+title+"|"+body)
			return "https://github.com/Smana/a/issues/7", nil
		},
		After: func(time.Duration) <-chan time.Time {
			polled <- struct{}{}
			c := make(chan time.Time, 1)
			c <- time.Now()
			return c
		}}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- relay.Serve(t.Context(), inR, outW); _ = outW.Close() }()
	out := bufio.NewScanner(outR)
	send := func(v any) {
		b, _ := json.Marshal(v)
		if _, err := inW.Write(append(b, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	next := func() map[string]any {
		if !out.Scan() {
			t.Fatal("the relay closed")
		}
		var m map[string]any
		_ = json.Unmarshal(out.Bytes(), &m)
		return m
	}
	ask := func(id int, method string, params any) map[string]any {
		send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		return next()
	}

	if res := ask(1, "initialize", map[string]any{"protocolVersion": "2025-11-25", "capabilities": map[string]any{"elicitation": map[string]any{}}}); !strings.Contains(fmt.Sprint(res), "tasks") {
		t.Fatalf("initialize: %v", res)
	}
	send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"})
	if tools := ask(2, "tools/list", nil)["result"].(map[string]any)["tools"].([]any); len(tools) != 7 {
		t.Fatalf("six broker tools and issue_file: %v", tools)
	}
	if res := ask(3, "tools/call", map[string]any{"name": "room_queue", "arguments": map[string]any{"room": roomID, "text": "use v2"}}); strings.Contains(fmt.Sprint(res), "isError:true") {
		t.Fatalf("queue: %v", res)
	}
	if !strings.HasPrefix(l.queue[0].Text, "⟦machine-sent via mcp") {
		t.Fatalf("queued %q", l.queue[0].Text)
	}

	created := ask(4, "tools/call", map[string]any{"name": "room_tail", "arguments": map[string]any{"room": roomID}, "task": map[string]any{"ttl": 60000}})
	id := created["result"].(map[string]any)["task"].(map[string]any)["taskId"]
	send(map[string]any{"jsonrpc": "2.0", "id": 5, "method": "tasks/result", "params": map[string]any{"taskId": id}})
	<-polled     // the relay saw the task working and waits
	e.log.add(1) // the room moves
	if res := next(); res["id"] != 5.0 || !strings.Contains(fmt.Sprint(res["result"]), "seq:11") {
		t.Fatalf("tasks/result: %v", res)
	}

	body := "## What is wrong\nx\u200b\n## What it should be\ny\n## Acceptance check\nz"
	send(map[string]any{"jsonrpc": "2.0", "id": 6, "method": "tools/call", "params": map[string]any{"name": "issue_file",
		"arguments": map[string]any{"repo": "Smana/a", "title": "docs: a dead link", "body": body}}})
	el := next()
	if el["method"] != "elicitation/create" {
		t.Fatalf("%v", el)
	}
	send(map[string]any{"jsonrpc": "2.0", "id": el["id"], "result": map[string]any{"action": "accept", "content": map[string]any{"file": true}}})
	if res := next(); !strings.Contains(fmt.Sprint(res["result"]), "issues/7") {
		t.Fatalf("issue_file: %v", res)
	}
	mu.Lock()
	if len(filed) != 1 || filed[0] != "Smana/a|docs: a dead link|## What is wrong\nx\n## What it should be\ny\n## Acceptance check\nz" {
		t.Fatalf("filed %q", filed)
	}
	mu.Unlock()
	_ = inW.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
