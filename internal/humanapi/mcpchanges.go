// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/mcpwire"
	"github.com/Smana/agent-platform/internal/roomctrl"
	"github.com/Smana/agent-platform/internal/roomview"
)

// changesWindow is how far back room_changes looks without a cursor.
const changesWindow = 24 * time.Hour

type changesArgs struct {
	Room   string `json:"room"`
	Cursor string `json:"cursor"`
}

// room_changes is view 4: the room's milestones after a cursor, the last 24 hours without one.
func (s *Server) roomChanges(ctx context.Context, p authn.Principal, args json.RawMessage) (any, error) {
	var a changesArgs
	if mcpwire.DecodeArgs(args, &a) != nil || a.Room == "" {
		return nil, toolError("invalid_arguments: room is a room id; cursor is seq:N from the last page")
	}
	after, hasCursor := int64(0), a.Cursor != ""
	if hasCursor {
		var ok bool
		if after, ok = parseCursor(a.Cursor); !ok {
			return nil, toolError("invalid_arguments: room is a room id; cursor is seq:N from the last page")
		}
	}
	room, st, _, err := s.readRoom(ctx, a.Room, p)
	if err != nil {
		return nil, err
	}
	if !hasCursor {
		after = max(st.LastSeq-tailScan, 0)
	}
	after = min(after, st.LastSeq)
	cursor := after
	var events []envelope.Event
	cutoff := s.now().Add(-changesWindow)
	for scanned := 0; scanned < tailScan && cursor < st.LastSeq; {
		n := int(min(int64(pageSize), int64(tailScan-scanned), st.LastSeq-cursor))
		evs, err := s.Log.Range(ctx, a.Room, cursor, n)
		if err != nil {
			s.log().Warn("changes: log unreadable", "room", a.Room, "err", err)
			return nil, errLogUnreadable
		}
		for _, e := range evs {
			cursor = e.Seq
			if !hasCursor && len(events) == 0 && e.TS.Before(cutoff) {
				after = e.Seq // older than the window: the baseline moves up to it
				continue
			}
			events = append(events, e)
		}
		scanned += len(evs)
		if len(evs) < n {
			break // the end of the log
		}
	}
	var base *envelope.TaskFacts
	raw, err := s.Log.TaskStateAt(ctx, a.Room, roomctrl.ForkSeq(room), after)
	if err != nil {
		s.log().Warn("changes: task facts unreadable", "room", a.Room, "err", err)
		return nil, errLogUnreadable
	}
	if raw != nil {
		var f envelope.TaskFacts
		if json.Unmarshal(raw, &f) == nil {
			base = &f
		}
	}
	set := roomview.Milestones(roomview.MilestoneInput{Room: a.Room, URL: s.PublicURL + "/r/" + a.Room, Base: base, Events: events})
	if set.Cursor == "" {
		set.Cursor = "seq:" + strconv.FormatInt(cursor, 10)
	}
	var text strings.Builder
	if err := roomview.Changes(&text, set, s.viewOptions(false)); err != nil {
		return nil, err
	}
	return viewResult{text: text.String(), data: set}, nil
}
