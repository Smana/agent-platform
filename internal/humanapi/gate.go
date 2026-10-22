// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/policy"
	"github.com/Smana/agent-platform/internal/store"
)

// A room read's refusals. Each reads the same whatever the room, so none tells an unreadable room
// from a missing one; the text is the HTTP body, and the MCP server's tool error.
var (
	errNoSuchRoom       = errors.New(noSuchRoom)
	errRoomsUnavailable = errors.New("rooms unavailable")
	errAccessUnverified = errors.New(reasonAccessUnverified)
	errLogUnavailable   = errors.New("log unavailable")
	errLogUnreadable    = errors.New("room log unreadable")
)

// roomStatus is a room read's refusal as an HTTP status: only a missing room is a 404, and every
// other one is worth a retry.
func roomStatus(err error) int {
	if errors.Is(err, errNoSuchRoom) {
		return http.StatusNotFound
	}
	return http.StatusServiceUnavailable
}

// gateRoom reads the room, checks that p may see it (D7), then reads its log row. Its errors are
// errNoSuchRoom, errRoomsUnavailable, errAccessUnverified and errLogUnavailable.
func (s *Server) gateRoom(ctx context.Context, id string, p authn.Principal) (*v1alpha1.Room, store.RoomState, error) {
	room, found, err := s.room(ctx, id)
	if err != nil {
		return nil, store.RoomState{}, errRoomsUnavailable
	}
	if !found {
		return nil, store.RoomState{}, errNoSuchRoom
	}
	// Before the log: a log error must not tell an unreadable room from a missing one.
	actx, acancel := context.WithTimeout(ctx, accessWait)
	ok, err := s.admits(actx, room, p)
	acancel()
	if err != nil {
		s.log().Warn("room access unverified", "room", id, "err", err)
		return nil, store.RoomState{}, errAccessUnverified
	}
	if !ok {
		return nil, store.RoomState{}, errNoSuchRoom
	}
	st, err := s.Log.Room(ctx, id)
	if errors.Is(err, store.ErrNoRoom) {
		return nil, store.RoomState{}, errNoSuchRoom
	}
	if err != nil {
		return nil, store.RoomState{}, errLogUnavailable
	}
	return room, st, nil
}

// readRoom is gateRoom, then the Read check on p's standing, which a room p may not read fails as
// a missing one.
func (s *Server) readRoom(ctx context.Context, id string, p authn.Principal) (*v1alpha1.Room, store.RoomState, policy.Subject, error) {
	room, st, err := s.gateRoom(ctx, id, p)
	if err != nil {
		return nil, st, policy.Subject{}, err
	}
	sub, _ := s.you(room, p, st.Driver)
	if !policy.Allowed(sub, policy.Read) {
		return nil, st, policy.Subject{}, errNoSuchRoom
	}
	return room, st, sub, nil
}
