// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"context"
	"net/http"
	"strconv"

	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/policy"
	"github.com/Smana/agent-platform/internal/summary"
)

// roomSummary is GET /api/rooms/{id}/summary?after=N: the summary/v1 fold of the room's log. It
// is a room read like the WebSocket, so it takes the same D7 gate and Read check before it
// touches the log, and an unreadable room is as missing as one that does not exist.
func (s *Server) roomSummary(w http.ResponseWriter, r *http.Request) {
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	var after int64
	if v := r.URL.Query().Get("after"); v != "" {
		var err error
		if after, err = strconv.ParseInt(v, 10, 64); err != nil || after < 0 {
			http.Error(w, "after is a sequence number", http.StatusBadRequest)
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), routeTimeout)
	defer cancel()
	view, err := s.summaryOf(ctx, r.PathValue("id"), p, after)
	if err != nil {
		http.Error(w, err.Error(), roomStatus(err))
		return
	}
	writeJSON(w, view)
}

// summaryOf is the room's summary/v1 as p sees it, notes after after: readRoom's refusals, or
// errLogUnreadable.
func (s *Server) summaryOf(ctx context.Context, id string, p authn.Principal, after int64) (summary.Summary, error) {
	room, _, sub, err := s.readRoom(ctx, id, p)
	if err != nil {
		return summary.Summary{}, err
	}
	// Each page is folded, then dropped: the read holds one page, whatever the log's length.
	var fold summary.State
	for last := int64(0); ; {
		page, err := s.Log.Range(ctx, id, last, pageSize)
		if err != nil {
			s.log().Warn("summary: log unreadable", "room", id, "err", err)
			return summary.Summary{}, errLogUnreadable
		}
		fold.Add(page)
		if len(page) < pageSize {
			break
		}
		last = page[len(page)-1].Seq
	}
	view := summary.View(fold, id, s.PublicURL+"/r/"+id, sub, after, s.now())
	s.warnNoRepository(view, fold.Facts, id, sub)
	// The fold's task phase is fresher than the Room's projection; a sealed log is a closed room.
	task, phase := "", room.Status.Phase
	if fold.Facts != nil {
		task = fold.Facts.Phase
	}
	if fold.Sealed != "" {
		phase = "Closed"
	}
	view.Status.State = summary.Coarse(task, phase, s.nextRun(id))
	return view, nil
}

// warnNoRepository logs what the pure fold no longer does: the task facts name no repository, so
// the stop action is withheld (when the viewer may close the task) or the merge need falls back
// to the bypass-only text.
func (s *Server) warnNoRepository(v summary.Summary, f *envelope.TaskFacts, room string, sub policy.Subject) {
	if summary.Repo(f) != "" {
		return
	}
	merge := false
	for _, n := range v.NeedsYou {
		merge = merge || n.Kind == "merge"
	}
	if (f != nil && f.Issue != nil && policy.Allowed(sub, policy.Close)) || merge {
		s.log().Warn("summary: task facts name no repository", "room", room)
	}
}
