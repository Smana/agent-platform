// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"context"
	"net/http"
	"strconv"
	"time"

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
	id := r.PathValue("id")
	room, st, ok := s.lookup(w, r, id, p)
	if !ok {
		return
	}
	sub, _ := s.you(room, p, st.Driver)
	if !policy.Allowed(sub, policy.Read) {
		http.Error(w, noSuchRoom, http.StatusNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), routeTimeout)
	defer cancel()
	// Each page is folded, then dropped: the read holds one page, whatever the log's length.
	var fold summary.State
	for last := int64(0); ; {
		page, err := s.Log.Range(ctx, id, last, pageSize)
		if err != nil {
			s.log().Warn("summary: log unreadable", "room", id, "err", err)
			http.Error(w, "room log unreadable", http.StatusServiceUnavailable)
			return
		}
		fold.Add(page)
		if len(page) < pageSize {
			break
		}
		last = page[len(page)-1].Seq
	}
	writeJSON(w, summary.View(fold, id, s.PublicURL+"/r/"+id, sub, after, time.Now()))
}
