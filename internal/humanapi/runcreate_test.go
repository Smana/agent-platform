// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/fanout"
	"github.com/Smana/agent-platform/internal/runrequest"
)

// fakeTaskFactory answers CreateTask as the factory would, and keeps every request.
type fakeTaskFactory struct {
	mu     sync.Mutex
	answer runrequest.TaskResult
	err    error
	got    []runrequest.TaskRequest
}

func (f *fakeTaskFactory) CreateTask(_ context.Context, r runrequest.TaskRequest) (runrequest.TaskResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.got = append(f.got, r)
	return f.answer, f.err
}

func withTaskFactory(f TaskCreator) option {
	return func(s *Server, _ *fanout.Hub, _ *hubView) { s.Tasks = f }
}

// setTaskPhase appends the factory's task_state with phase, as the factory narrates one.
func setTaskPhase(e env, phase string) {
	e.log.mu.Lock()
	defer e.log.mu.Unlock()
	e.log.evs = append(e.log.evs, logEvent(int64(len(e.log.evs)+1), envelope.StateChanged,
		envelope.Actor{Kind: envelope.ActorSystem, ID: "system:factory"}, envelope.TaskStatePayload(envelope.TaskFacts{Phase: phase})))
}

// getTask is tasks/get's answer for id, as h, and the JSON-RPC error's code (0 on success).
func getTask(t *testing.T, e env, h http.Header, id string) (taskView, int) {
	t.Helper()
	_, out := rpc(t, e, h, "tasks/get", map[string]any{"taskId": id})
	var v taskView
	b, _ := json.Marshal(out["result"])
	_ = json.Unmarshal(b, &v)
	return v, rpcCode(out)
}

// startRunTask calls run_create as a task and returns the CreateTaskResult's task.
func startRunTask(t *testing.T, e env, h http.Header, args map[string]any) taskView {
	t.Helper()
	_, out := rpc(t, e, h, "tools/call", map[string]any{"name": "run_create", "arguments": args, "task": map[string]any{"ttl": 86400000}})
	var res struct{ Task taskView }
	b, _ := json.Marshal(out["result"])
	if json.Unmarshal(b, &res) != nil || res.Task.TaskID == "" {
		t.Fatalf("not a task: %v", out)
	}
	return res.Task
}

const runDraft = "## What is wrong\nx\n## What it should be\ny\n## Acceptance check\nz"

// F4, F5, R14: run_create is refused without max_cost and with a secret in its draft, before the
// factory is asked; with both right, the answer is a task whose status follows the room's coarse
// state, bound to its caller.
func TestRunCreate(t *testing.T) {
	f := &fakeTaskFactory{answer: runrequest.TaskResult{Task: roomID, Room: roomID, Issue: 42}}
	ci := authn.Principal{Kind: envelope.ActorSystem, ID: "system:ci", Sponsored: true, AccessToken: "u", Expiry: sponsoredP.Expiry}
	e := setup(t, mcpOn, actorOn(&actLog{}), withSponsored(sponsoredP, ci), withTaskFactory(f), sponsoredRooms(t, world(nil)))
	h := bearer("slack-app")
	if got := call(t, e, h, "run_create", map[string]any{"repo": "Smana/ai-native-ref", "text": "x", "dataClass": "public"}); !got.isError ||
		!strings.Contains(got.text, "max_cost") {
		t.Fatalf("no max_cost: %+v", got)
	}
	if got := call(t, e, h, "run_create", map[string]any{"repo": "Smana/ai-native-ref", "title": "docs(x): y",
		"text": runDraft + "\ntoken SECRET", "dataClass": "public", "max_cost": 2}); !got.isError || !strings.Contains(got.text, "looks like a secret") {
		t.Fatalf("a secret in a titled draft: %+v", got)
	}
	if len(f.got) != 0 {
		t.Fatal("asked the factory")
	}
	task := startRunTask(t, e, h, map[string]any{"repo": "Smana/ai-native-ref", "title": "docs(x): y", "text": runDraft,
		"dataClass": "public", "max_cost": 2, "on_behalf_of": "slack:T01ABC/U02DEF"})
	if !strings.HasPrefix(task.TaskID, "run.") || task.Status != "working" {
		t.Fatalf("%+v", task)
	}
	if got := f.got[0]; got.AccessToken != "t" || got.MaxCostUSD != 2 || got.OnBehalfOf != "slack:T01ABC/U02DEF" ||
		!strings.HasPrefix(got.IdempotencyKey, "system:slack-app:") {
		t.Fatalf("%+v", got)
	}
	for _, c := range []struct{ phase, want string }{
		{"AwaitingHuman", "input_required"}, {"Done", "completed"}, {"Rejected", "failed"}, {"Stopped", "cancelled"},
	} {
		setTaskPhase(e, c.phase)
		if got, _ := getTask(t, e, h, task.TaskID); got.Status != c.want {
			t.Errorf("%s: %s, want %s", c.phase, got.Status, c.want)
		}
	}
	setTaskPhase(e, "AwaitingHuman")
	if got, _ := getTask(t, e, h, task.TaskID); !strings.Contains(got.StatusMessage, "/r/"+roomID) {
		t.Fatalf("a wait names the room page, never a command: %q", got.StatusMessage)
	}
	if _, code := getTask(t, e, bearer("ci"), task.TaskID); code != -32602 {
		t.Fatalf("another principal's task: %d", code)
	}
}

// C19: the room exists once triage makes it; until then, and for two minutes, the task is working.
func TestRunTaskBeforeItsRoom(t *testing.T) {
	base := time.Now()
	var later atomic.Int64 // nanoseconds past base: the handler reads the clock on its own goroutine
	f := &fakeTaskFactory{answer: runrequest.TaskResult{Task: roomB, Room: roomB}}
	e := setup(t, mcpOn, actorOn(&actLog{}), withSponsored(sponsoredP), withTaskFactory(f),
		func(s *Server, _ *fanout.Hub, _ *hubView) {
			s.Rooms = roomClient(t)
			s.Now = func() time.Time { return base.Add(time.Duration(later.Load())) }
		})
	task := startRunTask(t, e, bearer("slack-app"), map[string]any{"repo": "Smana/ai-native-ref", "text": "x", "dataClass": "public", "max_cost": 1})
	if task.Status != "working" || !strings.Contains(task.StatusMessage, "not created yet") {
		t.Fatalf("%+v", task)
	}
	later.Store(int64(3 * time.Minute))
	if got, _ := getTask(t, e, bearer("slack-app"), task.TaskID); got.Status != "failed" {
		t.Fatalf("past the grace: %+v", got)
	}
}

// The factory's refusal code reaches the caller unchanged; a human never sees run_create; a long
// idempotency_key and a non-task call are refused.
func TestRunCreateRefusalsAndGate(t *testing.T) {
	f := &fakeTaskFactory{err: runrequest.ErrRefused{Status: 403, Reason: "not_linked"}}
	e := setup(t, mcpOn, actorOn(&actLog{}), withSponsored(sponsoredP), withTaskFactory(f), sponsoredRooms(t, world(nil)))
	args := map[string]any{"repo": "Smana/ai-native-ref", "text": "x", "dataClass": "public", "max_cost": 1}
	if got := call(t, e, bearer("slack-app"), "run_create", args); !got.isError || !strings.Contains(got.text, "not_linked") {
		t.Fatalf("%+v", got)
	}
	_, out := rpc(t, e, bearer("slack-app"), "tools/call", map[string]any{"name": "run_create", "arguments": args, "task": map[string]any{}})
	if !strings.Contains(strings.ToLower(jsonOf(out)), "not_linked") {
		t.Fatalf("as a task: %v", out)
	}
	long := map[string]any{"repo": "Smana/ai-native-ref", "text": "x", "dataClass": "public", "max_cost": 1,
		"idempotency_key": strings.Repeat("k", 129)}
	if got := call(t, e, bearer("slack-app"), "run_create", long); !got.isError {
		t.Fatalf("a 129-char key: %+v", got)
	}
	_, hout := rpc(t, e, viaRoomctl("dev"), "tools/call", map[string]any{"name": "run_create", "arguments": args})
	if rpcCode(hout) != -32602 || !strings.Contains(jsonOf(hout), "unknown tool: run_create") {
		t.Fatalf("a human: %v", hout)
	}
	if strings.Contains(toolNames(t, e, viaRoomctl("dev")), "run_create") {
		t.Fatal("a human lists run_create")
	}
}

func jsonOf(v any) string { b, _ := json.Marshal(v); return string(b) }

// M3: the advertised schema is enforced by the server: a bad ref or on_behalf_of never reaches the factory.
func TestRunCreateChecksRefAndSponsor(t *testing.T) {
	f := &fakeTaskFactory{answer: runrequest.TaskResult{Task: roomID, Room: roomID}}
	e := setup(t, mcpOn, actorOn(&actLog{}), withSponsored(sponsoredP), withTaskFactory(f), sponsoredRooms(t, world(nil)))
	base := func(k, v string) map[string]any {
		return map[string]any{"repo": "Smana/ai-native-ref", "text": "x", "dataClass": "public", "max_cost": 1, k: v}
	}
	for k, vs := range map[string][]string{
		"ref":          {"-x", "a b", "a..b", "a\nb", strings.Repeat("a", 257), "x;rm"},
		"on_behalf_of": {"slack:T01ABC", "U02DEF", "slack:t01abc/u02def", "slack:T01ABC/U02DEF\nx"},
	} {
		for _, v := range vs {
			if got := call(t, e, bearer("slack-app"), "run_create", base(k, v)); !got.isError {
				t.Errorf("%s=%q: %+v", k, v, got)
			}
		}
	}
	if len(f.got) != 0 {
		t.Fatal("asked the factory")
	}
	if got := call(t, e, bearer("slack-app"), "run_create", base("ref", "refs/heads/main")); got.isError {
		t.Fatalf("a good ref: %+v", got)
	}
}

// M5: redaction of untitled text, fail closed, and the task ids' bindings.
func TestRunCreateUntitledAndIDs(t *testing.T) {
	f := &fakeTaskFactory{answer: runrequest.TaskResult{Task: roomID, Room: roomID}}
	args := map[string]any{"repo": "Smana/ai-native-ref", "text": "token SECRET", "dataClass": "public", "max_cost": 1}
	e := setup(t, mcpOn, actorOn(&actLog{}), withSponsored(sponsoredP), withTaskFactory(f), sponsoredRooms(t, world(nil)))
	if got := call(t, e, bearer("slack-app"), "run_create", args); !got.isError || !strings.Contains(got.text, "looks like a secret") {
		t.Fatalf("untitled secret: %+v", got)
	}
	nored := setup(t, mcpOn, withSponsored(sponsoredP), withTaskFactory(f), sponsoredRooms(t, world(nil)))
	args["text"] = "fine"
	if got := call(t, nored, bearer("slack-app"), "run_create", args); !got.isError || !strings.Contains(got.text, "unavailable") {
		t.Fatalf("no redactor: %+v", got)
	}
	if len(f.got) != 0 {
		t.Fatal("asked the factory")
	}
	// A principal with no token gets unavailable, never a forwarded empty bearer.
	notoken := sponsoredP
	notoken.AccessToken = ""
	inner := &fakeTaskFactory{}
	et := setup(t, mcpOn, actorOn(&actLog{}), withSponsored(notoken), withTaskFactory(tokenCheck{inner}), sponsoredRooms(t, world(nil)))
	if got := call(t, et, bearer("slack-app"), "run_create", args); !got.isError || !strings.Contains(got.text, "unavailable") {
		t.Fatalf("no access token: %+v", got)
	}
	// Task ids: a run. id is a sponsored principal's only; the prefixes never cross.
	ok := startRunTask(t, e, bearer("slack-app"), map[string]any{"repo": "Smana/ai-native-ref", "text": "fine", "dataClass": "public", "max_cost": 1})
	payload := strings.TrimPrefix(ok.TaskID, "run.")
	human := runTask{V: 1, By: "human:dev", Room: roomID, Created: time.Now().UnixMilli(), TTL: time.Hour.Milliseconds()}
	for _, id := range []string{human.id(), "tail." + payload, "run." + strings.TrimPrefix(
		tailTask{V: 1, By: "system:slack-app", Room: roomID, Created: time.Now().UnixMilli(), TTL: time.Minute.Milliseconds()}.id(), "tail.")} {
		if _, code := getTask(t, e, bearer("slack-app"), id); code != -32602 {
			t.Errorf("%.20s: %d", id, code)
		}
	}
	if _, code := getTask(t, e, viaRoomctl("dev"), ok.TaskID); code != -32602 {
		t.Fatalf("a human with a run id: %d", code)
	}
}

// tokenCheck refuses as runrequest.Factory does without a token.
type tokenCheck struct{ f *fakeTaskFactory }

func (c tokenCheck) CreateTask(ctx context.Context, r runrequest.TaskRequest) (runrequest.TaskResult, error) {
	if r.AccessToken == "" {
		return runrequest.TaskResult{}, runrequest.ErrForbidden
	}
	return c.f.CreateTask(ctx, r)
}
