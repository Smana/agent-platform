// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Smana/agent-platform/internal/fanout"
)

// F3: the tools mirror roomctl's standing; none labels, approves, steers, stops or starts work.
func TestMCPToolList(t *testing.T) {
	e := setup(t, mcpOn)
	_, out := rpc(t, e, viaRoomctl("dev"), "tools/list", nil)
	var res struct {
		Tools []struct {
			Name      string
			Execution *struct{ TaskSupport string }
		}
	}
	b, _ := json.Marshal(out["result"])
	_ = json.Unmarshal(b, &res)
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
		if (tool.Execution != nil) != (tool.Name == "room_tail") || tool.Execution != nil && tool.Execution.TaskSupport != "optional" {
			t.Errorf("%s: execution %+v", tool.Name, tool.Execution)
		}
	}
	if want := []string{"rooms_list", "room_summary", "room_tail", "room_queue", "room_fork", "issue_draft"}; !slices.Equal(names, want) {
		t.Fatalf("tools %v, want %v", names, want)
	}
}

// taskOf sends room_tail as a task and returns the CreateTaskResult's task.
func taskOf(t *testing.T, e env, h http.Header, args, task map[string]any) taskView {
	t.Helper()
	_, out := rpc(t, e, h, "tools/call", map[string]any{"name": "room_tail", "arguments": args, "task": task})
	var res struct{ Task taskView }
	b, _ := json.Marshal(out["result"])
	if json.Unmarshal(b, &res) != nil || res.Task.TaskID == "" {
		t.Fatalf("not a task: %v", out)
	}
	return res.Task
}

// F5: room_tail runs as an MCP Task that waits for the room's next events, bound to its caller.
func TestMCPTailTask(t *testing.T) {
	now := time.Now()
	e := setup(t, mcpOn, func(s *Server, _ *fanout.Hub, _ *hubView) { s.Now = func() time.Time { return now } })
	_, out := rpc(t, e, viaRoomctl("dev"), "initialize", map[string]any{"protocolVersion": "2025-11-25"})
	if fmt.Sprint(out["result"].(map[string]any)["capabilities"].(map[string]any)["tasks"]) != "map[requests:map[tools:map[call:map[]]]]" {
		t.Fatalf("tasks capability: %v", out)
	}
	_, out = rpc(t, e, viaRoomctl("dev"), "initialize", map[string]any{"protocolVersion": "2025-06-18"})
	if _, ok := out["result"].(map[string]any)["capabilities"].(map[string]any)["tasks"]; ok {
		t.Fatal("2025-06-18 has no tasks")
	}

	task := taskOf(t, e, viaRoomctl("dev"), map[string]any{"room": roomID}, map[string]any{"ttl": 60000})
	if task.Status != "working" || task.TTL != 60000 || task.PollInterval != 5000 {
		t.Fatalf("%+v", task)
	}
	get := func(h http.Header, method string) map[string]any {
		_, out := rpc(t, e, h, method, map[string]any{"taskId": task.TaskID})
		return out
	}
	if out := get(viaRoomctl("dev"), "tasks/result"); rpcCode(out) != -32600 {
		t.Fatalf("a working task's result: %v", out)
	}
	if out := get(viaRoomctl("other"), "tasks/get"); rpcCode(out) != -32602 {
		t.Fatalf("another caller's task: %v", out)
	}
	e.log.add(1)
	if out := get(viaRoomctl("dev"), "tasks/get"); out["result"].(map[string]any)["status"] != "completed" {
		t.Fatalf("an event arrived: %v", out)
	}
	out = get(viaRoomctl("dev"), "tasks/result")
	res := out["result"].(map[string]any)
	if !strings.Contains(fmt.Sprint(res["structuredContent"]), "seq:11") ||
		fmt.Sprint(res["_meta"]) != "map[io.modelcontextprotocol/related-task:map[taskId:"+task.TaskID+"]]" {
		t.Fatalf("result: %v", out)
	}

	waiting := taskOf(t, e, viaRoomctl("dev"), map[string]any{"room": roomID}, map[string]any{})
	if waiting.TTL != tailWait.Milliseconds() {
		t.Fatalf("the default wait: %+v", waiting)
	}
	now = now.Add(tailWait)
	_, out = rpc(t, e, viaRoomctl("dev"), "tasks/get", map[string]any{"taskId": waiting.TaskID})
	if r := out["result"].(map[string]any); r["status"] != "completed" || r["statusMessage"] != "no event within the wait" {
		t.Fatalf("past the wait: %v", out)
	}

	if failed := taskOf(t, e, viaRoomctl("dev"), map[string]any{"room": "aaaaaaaa"}, map[string]any{}); failed.Status != "failed" || failed.StatusMessage != "no such room" {
		t.Fatalf("a missing room: %+v", failed)
	}
	if _, out := rpc(t, e, viaRoomctl("dev"), "tools/call", map[string]any{"name": "room_summary", "arguments": map[string]any{"room": roomID}, "task": map[string]any{}}); rpcCode(out) != -32601 {
		t.Fatalf("a tool that does not run as a task: %v", out)
	}
}
