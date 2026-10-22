// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/mcpwire"
	"github.com/Smana/agent-platform/internal/summary"
)

// room_tail as an MCP Task (F5): it waits for the room's next events. The broker keeps no task:
// the task id carries the query, any replica answers tasks/get from the log, and the result is a
// page read when asked. tasks/result never blocks: roomctl mcp, the only client /mcp admits,
// polls tasks/get first, so every request stays inside oauth2-proxy's 30 s.
const (
	tailWait      = 5 * time.Minute // a task's wait when it asks for no ttl
	minTailWait   = 10 * time.Second
	maxTailWait   = 15 * time.Minute
	taskSkew      = time.Minute     // how far ahead of this replica's clock a task may have been created
	taskPoll      = 5 * time.Second // the pollInterval a task suggests
	maxTaskID     = 2 << 10
	taskPrefix    = "tail."
	relatedTaskID = "io.modelcontextprotocol/related-task"
)

// The task statuses room_tail uses: never input_required, which only run_create (F4) will use.
const (
	taskWorking   = "working"
	taskCompleted = "completed"
	taskFailed    = "failed"
)

// tailTask is a room_tail task, carried whole in its id. By binds it to the principal that created
// it: another caller's id answers as a missing task. The id is an unauthenticated cursor, not a
// signed token: anyone can forge one for their own principal. That is harmless, because every read
// is authorized afresh under the caller's principal and decodeTask bounds every field, so a forged
// id can only shape the caller's own bounded query.
type tailTask struct {
	V       int      `json:"v"`
	By      string   `json:"by"`
	Room    string   `json:"room"`
	After   int64    `json:"after"`
	Kinds   []string `json:"kinds,omitempty"`
	Created int64    `json:"created"` // unix milliseconds
	TTL     int64    `json:"ttl"`     // milliseconds: the wait, and the task's life
}

func (t tailTask) id() string {
	b, _ := json.Marshal(t)
	return taskPrefix + base64.RawURLEncoding.EncodeToString(b)
}

func (t tailTask) deadline() time.Time { return time.UnixMilli(t.Created + t.TTL) }

// decodeTask reads id as a task of p's, or reports none.
func decodeTask(id string, p authn.Principal, now time.Time) (tailTask, bool) {
	raw, ok := strings.CutPrefix(id, taskPrefix)
	if !ok || len(id) > maxTaskID {
		return tailTask{}, false
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	var t tailTask
	if err != nil || mcpwire.DecodeArgs(b, &t) != nil || t.V != 1 || t.By != p.ID || !envelope.ValidID(t.Room) ||
		t.After < 0 || len(t.Kinds) > maxKinds || t.TTL < minTailWait.Milliseconds() || t.TTL > maxTailWait.Milliseconds() ||
		t.Created < now.Add(-maxTailWait).UnixMilli() || t.Created > now.Add(taskSkew).UnixMilli() {
		return tailTask{}, false
	}
	for _, k := range t.Kinds {
		if !envelope.Type(k).Valid() {
			return tailTask{}, false
		}
	}
	return t, true
}

// taskView is the Task object of tasks/get and of a CreateTaskResult.
type taskView struct {
	TaskID        string    `json:"taskId"`
	Status        string    `json:"status"`
	StatusMessage string    `json:"statusMessage"`
	CreatedAt     time.Time `json:"createdAt"`
	LastUpdatedAt time.Time `json:"lastUpdatedAt"`
	TTL           int64     `json:"ttl"`
	PollInterval  int64     `json:"pollInterval"`
}

// startTail is room_tail's tools/call with a task: the task, its status read at once. A task
// without a cursor waits for the events after the room's last seq now.
func (s *Server) startTail(ctx context.Context, w http.ResponseWriter, id json.RawMessage, p authn.Principal, args, task json.RawMessage) {
	a, after, err := tailArguments(args)
	var opts struct {
		TTL *int64 `json:"ttl"`
	}
	if err != nil || mcpwire.DecodeArgs(task, &opts) != nil {
		msg := "task takes {ttl?}, in milliseconds"
		if err != nil {
			msg = err.Error()
		}
		mcpwire.Write(w, id, nil, &mcpwire.Error{Code: mcpwire.InvalidParams, Message: msg})
		return
	}
	if after < 0 {
		_, st, _, err := s.readRoom(ctx, a.Room, p)
		switch {
		case errors.Is(err, errNoSuchRoom):
			after = 0 // the task fails as it is read, as it would with a cursor
		case err != nil:
			s.writeTaskError(ctx, w, id, err)
			return
		}
		after = max(after, st.LastSeq)
	}
	wait := tailWait
	if opts.TTL != nil {
		wait = time.Duration(min(max(*opts.TTL, minTailWait.Milliseconds()), maxTailWait.Milliseconds())) * time.Millisecond
	}
	t := tailTask{V: 1, By: p.ID, Room: a.Room, After: after, Kinds: a.Kinds, Created: s.now().UnixMilli(), TTL: wait.Milliseconds()}
	view, _, err := s.taskStatus(ctx, p, t)
	if err != nil {
		s.writeTaskError(ctx, w, id, err)
		return
	}
	s.mcpCounted(ctx, "room_tail", "ok")
	mcpwire.Write(w, id, map[string]any{"task": view}, nil)
}

// taskStatus is t's status now, and the result tasks/result returns once it is terminal. Only a
// room read's transient refusals are errors: a missing or unreadable room fails the task.
func (s *Server) taskStatus(ctx context.Context, p authn.Principal, t tailTask) (taskView, map[string]any, error) {
	now := s.now()
	view := taskView{TaskID: t.id(), Status: taskWorking, CreatedAt: time.UnixMilli(t.Created).UTC(), LastUpdatedAt: now.UTC(),
		TTL: t.TTL, PollInterval: taskPoll.Milliseconds()}
	page, err := s.tail(ctx, p, t.Room, t.After, t.Kinds)
	switch {
	case errors.Is(err, errNoSuchRoom):
		view.Status, view.StatusMessage = taskFailed, noSuchRoom
		return view, mcpwire.ToolError(noSuchRoom), nil
	case err != nil:
		return view, nil, err
	}
	res, err := mcpwire.ToolResult(page)
	if err != nil {
		return view, nil, err
	}
	final := page.State == summary.StateDone || page.State == summary.StateFailed || page.State == summary.StateStopped
	switch {
	case len(page.Events.Items) > 0 || page.More:
		view.Status, view.StatusMessage = taskCompleted, "events after the cursor"
	case final:
		view.Status, view.StatusMessage = taskCompleted, "the room is "+page.State
	case !now.Before(t.deadline()):
		view.Status, view.StatusMessage = taskCompleted, "no event within the wait"
	default:
		view.StatusMessage = "waiting for events; the room is " + page.State
	}
	return view, res, nil
}

// taskParams reads tasks/get's and tasks/result's {taskId} as a task of p's, or writes -32602.
func taskParams(w http.ResponseWriter, req mcpwire.Request, p authn.Principal, now time.Time) (tailTask, bool) {
	var a struct {
		TaskID string `json:"taskId"`
	}
	t, ok := tailTask{}, false
	if json.Unmarshal(req.Params, &a) == nil {
		t, ok = decodeTask(a.TaskID, p, now)
	}
	if !ok {
		mcpwire.Write(w, req.ID, nil, &mcpwire.Error{Code: mcpwire.InvalidParams, Message: "no such task"})
	}
	return t, ok
}

// tasksGet is tasks/get: the task's status, read from the log.
func (s *Server) tasksGet(ctx context.Context, w http.ResponseWriter, req mcpwire.Request, p authn.Principal) {
	t, ok := taskParams(w, req, p, s.now())
	if !ok {
		return
	}
	view, _, err := s.taskStatus(ctx, p, t)
	if err != nil {
		s.writeTaskError(ctx, w, req.ID, err)
		return
	}
	mcpwire.Write(w, req.ID, view, nil)
}

// tasksResult is tasks/result: the page of a terminal task, tied to it by _meta. A working task is
// -32600: poll tasks/get, as roomctl mcp does.
func (s *Server) tasksResult(ctx context.Context, w http.ResponseWriter, req mcpwire.Request, p authn.Principal) {
	t, ok := taskParams(w, req, p, s.now())
	if !ok {
		return
	}
	view, res, err := s.taskStatus(ctx, p, t)
	switch {
	case err != nil:
		s.writeTaskError(ctx, w, req.ID, err)
	case view.Status == taskWorking:
		mcpwire.Write(w, req.ID, nil, &mcpwire.Error{Code: mcpwire.InvalidRequest, Message: "task_working: poll tasks/get until it completes"})
	default:
		res["_meta"] = map[string]any{relatedTaskID: map[string]string{"taskId": view.TaskID}}
		mcpwire.Write(w, req.ID, res, nil)
	}
}

// writeTaskError answers a task request that could not be read: a room read's refusal by its
// text, which reads the same for every room, anything else as unavailable. Both are worth a retry.
func (s *Server) writeTaskError(ctx context.Context, w http.ResponseWriter, id json.RawMessage, err error) {
	msg, ok := refusal(err)
	if !ok {
		s.log().Error("mcp task read failed", "err", err)
		msg = "unavailable: try again later"
	}
	s.mcpCounted(ctx, "room_tail", "error")
	mcpwire.Write(w, id, nil, &mcpwire.Error{Code: mcpwire.InternalError, Message: msg})
}
