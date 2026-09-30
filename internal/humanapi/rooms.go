// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"context"
	"net/http"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/policy"
	"github.com/Smana/agent-platform/internal/wire"
)

func roleName(r policy.Role) string {
	switch r {
	case policy.Watcher:
		return "watcher"
	case policy.Collaborator:
		return "collaborator"
	case policy.Owner:
		return "owner"
	}
	return "none"
}

// you resolves p's standing in room. Only the web client is the web UI; an
// unreadable client id ("") makes nobody one.
func (s *Server) you(room *v1alpha1.Room, p authn.Principal, driver string) (policy.Subject, wire.You) {
	web := s.WebClient()
	sub := s.Groups.Resolve(room, p, driver, web != "" && p.ClientID == web)
	return sub, wire.You{Principal: p.ID, Role: roleName(sub.Role), Approver: sub.Approver, Driver: sub.Driver, WebUI: sub.WebUI}
}

// roomRow is one row of GET /api/rooms.
type roomRow struct {
	ID        string   `json:"id"`
	Phase     string   `json:"phase"`
	Owner     string   `json:"owner"`
	Driver    string   `json:"driver"`
	DataClass string   `json:"dataClass"`
	LastSeq   int64    `json:"lastSeq"`
	You       wire.You `json:"you"`
}

// listRooms is GET /api/rooms: every room of the namespace the caller may read
// (agents-member reads all, §1 Groups), from the Room CRs' projected status.
func (s *Server) listRooms(w http.ResponseWriter, r *http.Request) {
	p, ok := s.principal(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), routeTimeout)
	defer cancel()
	var rooms v1alpha1.RoomList
	if err := s.Rooms.List(ctx, &rooms, client.InNamespace(s.Namespace)); err != nil {
		http.Error(w, "rooms unavailable", http.StatusServiceUnavailable)
		return
	}
	out := []roomRow{}
	for i := range rooms.Items {
		room := &rooms.Items[i]
		sub, you := s.you(room, p, room.Status.Driver)
		if !policy.Allowed(sub, policy.Read) {
			continue
		}
		out = append(out, roomRow{ID: room.Name, Phase: room.Status.Phase, Owner: room.Spec.Owner, Driver: room.Status.Driver,
			DataClass: room.Spec.DataClass, LastSeq: room.Status.LastSeq, You: you})
	}
	writeJSON(w, out)
}
