// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"

	"github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/mcpwire"
	"github.com/Smana/agent-platform/internal/summary"
)

// The tail's bounds: a page is what one tools/call returns.
const (
	tailFirst  = 50        // events a tail without a cursor starts with, as roomctl watch
	tailMax    = 100       // events a page holds
	tailBudget = 256 << 10 // payload bytes a page holds past its first event
	tailScan   = 2000      // events one page scans for its kinds
	maxKinds   = 10
)

// The read tools' arguments.
type (
	listArgs struct {
		Repo    string `json:"repo"`
		Mine    bool   `json:"mine"`
		NeedsMe bool   `json:"needsMe"`
	}
	summaryArgs struct {
		Room  string `json:"room"`
		After string `json:"after"`
	}
	tailArgs struct {
		Room   string   `json:"room"`
		Cursor string   `json:"cursor"`
		Kinds  []string `json:"kinds"`
	}
)

// tailPage is room_tail's result: the room's events after a cursor, which agents and humans wrote.
type tailPage struct {
	Room   string `json:"room"`
	State  string `json:"state"`
	Events struct {
		Untrusted bool             `json:"untrusted"`
		Items     []envelope.Event `json:"items"`
	} `json:"events"`
	Cursor string `json:"cursor"` // seq:N, the last event scanned: pass it back as is
	More   bool   `json:"more"`   // the log held more than this page when it was read
}

// rooms_list is GET /api/rooms as a tool: the rows, and why the list may be short.
func (s *Server) roomsList(ctx context.Context, p authn.Principal, args json.RawMessage) (any, error) {
	var a listArgs
	if mcpwire.DecodeArgs(args, &a) != nil || (a.Repo != "" && !repositoryRE.MatchString(a.Repo)) {
		return nil, toolError("invalid_arguments: repo is owner/name; mine and needsMe are booleans")
	}
	rows, access, err := s.roomRows(ctx, p, &roomFilter{repo: a.Repo, mine: a.Mine, needsMe: a.NeedsMe})
	if err != nil {
		return nil, err
	}
	return map[string]any{"rooms": rows, "access": access}, nil
}

// room_summary is GET /api/rooms/{id}/summary as a tool.
func (s *Server) roomSummaryTool(ctx context.Context, p authn.Principal, args json.RawMessage) (any, error) {
	var a summaryArgs
	bad := toolError("invalid_arguments: room is a room id; after is a cursor, seq:N")
	if mcpwire.DecodeArgs(args, &a) != nil || a.Room == "" {
		return nil, bad
	}
	after := int64(0)
	if a.After != "" {
		var ok bool
		if after, ok = parseCursor(a.After); !ok {
			return nil, bad
		}
	}
	return s.summaryOf(ctx, a.Room, p, after)
}

// room_tail is the room's events after a cursor, a page at a time: what roomctl watch prints.
func (s *Server) roomTail(ctx context.Context, p authn.Principal, args json.RawMessage) (any, error) {
	a, after, err := tailArguments(args)
	if err != nil {
		return nil, err
	}
	return s.tail(ctx, p, a.Room, after, a.Kinds)
}

// tailArguments reads room_tail's arguments; after is -1 without a cursor.
func tailArguments(args json.RawMessage) (tailArgs, int64, error) {
	var a tailArgs
	bad := toolError("invalid_arguments: room is a room id; cursor is seq:N from the last page; kinds are at most 10 event types")
	if mcpwire.DecodeArgs(args, &a) != nil || a.Room == "" || len(a.Kinds) > maxKinds {
		return a, 0, bad
	}
	for _, k := range a.Kinds {
		if !envelope.Type(k).Valid() {
			return a, 0, bad
		}
	}
	after := int64(-1)
	if a.Cursor != "" {
		var ok bool
		if after, ok = parseCursor(a.Cursor); !ok {
			return a, 0, bad
		}
	}
	return a, after, nil
}

// tail reads one page of room id after after (the last tailFirst events when after is -1), of
// kinds only when there are some.
func (s *Server) tail(ctx context.Context, p authn.Principal, id string, after int64, kinds []string) (tailPage, error) {
	room, st, _, err := s.readRoom(ctx, id, p)
	if err != nil {
		return tailPage{}, err
	}
	if after < 0 {
		after = max(st.LastSeq-tailFirst, 0)
	}
	page := tailPage{Room: id, State: s.coarse(room, st.Sealed)}
	page.Events.Untrusted, page.Events.Items = true, []envelope.Event{}
	last, size := min(after, st.LastSeq), 0
	full := false
	for scanned := 0; scanned < tailScan && last < st.LastSeq && !full; {
		n := int(min(int64(pageSize), int64(tailScan-scanned), st.LastSeq-last))
		evs, err := s.Log.Range(ctx, id, last, n)
		if err != nil {
			s.log().Warn("tail: log unreadable", "room", id, "err", err)
			return tailPage{}, errLogUnreadable
		}
		for _, e := range evs {
			if len(kinds) == 0 || slices.Contains(kinds, string(e.Type)) {
				if n := len(page.Events.Items); n == tailMax || (n > 0 && size+len(e.Payload) > tailBudget) {
					full = true
					break
				}
				page.Events.Items = append(page.Events.Items, e)
				size += len(e.Payload)
			}
			last = e.Seq
		}
		scanned += len(evs)
		if len(evs) < n {
			break // the end of the log
		}
	}
	page.Cursor, page.More = "seq:"+strconv.FormatInt(last, 10), last < st.LastSeq
	return page, nil
}

// coarse is room's coarse state from its projected status; a sealed log is a closed room.
func (s *Server) coarse(room *v1alpha1.Room, sealed bool) string {
	task, phase := "", room.Status.Phase
	if t := room.Status.Task; t != nil {
		task = t.Phase
	}
	if sealed {
		phase = "Closed"
	}
	return summary.Coarse(task, phase, s.nextRun(room.Name))
}

// parseCursor reads seq:N, or N, as roomctl status --after does.
func parseCursor(c string) (int64, bool) {
	n, err := strconv.ParseInt(strings.TrimPrefix(c, "seq:"), 10, 64)
	return n, err == nil && n >= 0
}
