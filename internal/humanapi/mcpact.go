// SPDX-License-Identifier: Apache-2.0

package humanapi

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/Smana/maku/internal/authn"
	"github.com/Smana/maku/internal/mcpwire"
	"github.com/Smana/maku/internal/runrequest"
	"github.com/Smana/maku/internal/wire"
)

// The write tools' arguments.
type (
	queueArgs struct {
		Room string `json:"room"`
		Text string `json:"text"`
	}
	forkArgs struct {
		Room   string   `json:"room"`
		At     int64    `json:"at"`
		Role   string   `json:"role"`
		PRURL  string   `json:"prUrl"`
		Egress []string `json:"egress"`
		Note   string   `json:"note"`
	}
)

// room_queue queues text for the room's next run, as roomctl post --queue does: an act through
// the MCP client, so the text is stored wrapped (F8) and the standing is roomctl's (P18).
func (s *Server) roomQueue(ctx context.Context, p authn.Principal, args json.RawMessage) (any, error) {
	var a queueArgs
	if mcpwire.DecodeArgs(args, &a) != nil || a.Room == "" || a.Text == "" {
		return nil, toolError("invalid_arguments: room is a room id; text is the note, at most 16 KiB")
	}
	ack, err := s.mcpAct(ctx, p, a.Room, Action{Kind: "message", Delivery: "queued", Text: a.Text})
	if err != nil {
		return nil, err
	}
	return map[string]any{"room": a.Room, "queued": ack.Seq}, nil
}

// room_fork forks the room at a seq into one the caller owns, with an optional first run, as
// roomctl fork does.
func (s *Server) roomFork(ctx context.Context, p authn.Principal, args json.RawMessage) (any, error) {
	var a forkArgs
	if mcpwire.DecodeArgs(args, &a) != nil || a.Room == "" || a.At < 1 {
		return nil, toolError("invalid_arguments: room is a room id; at is the last seq to copy, 1 or more; " +
			"role is implementer, reviewer or triager; egress names pypi, npm, golang or crates")
	}
	ack, err := s.mcpAct(ctx, p, a.Room, Action{Kind: "fork", Seq: a.At, Role: a.Role, PRURL: a.PRURL, EgressProfiles: a.Egress, Note: a.Note})
	if err != nil {
		return nil, err
	}
	var res struct {
		RoomID   string          `json:"roomId"`
		Run      json.RawMessage `json:"run"`
		RunError string          `json:"runError"`
	}
	if err := json.Unmarshal(ack.Result, &res); err != nil {
		return nil, err
	}
	out := map[string]any{"room": res.RoomID, "url": s.PublicURL + "/r/" + res.RoomID}
	if len(res.Run) > 0 && string(res.Run) != "null" {
		out["run"] = res.Run
	}
	if res.RunError != "" {
		out["runError"] = res.RunError
	}
	return out, nil
}

// mcpAct sends one act to room as p through the MCP client, after the room read's checks, and
// returns its ack; a refused act is a toolError naming the ack's reason (docs/api.md, Actions).
// Each call is its own session: MCP carries no retry key, so a retried call acts twice.
func (s *Server) mcpAct(ctx context.Context, p authn.Principal, id string, act Action) (wire.ServerFrame, error) {
	room, _, _, err := s.readRoom(ctx, id, p)
	if err != nil {
		return wire.ServerFrame{}, err
	}
	if s.Actor == nil {
		return wire.ServerFrame{}, errors.New("humanapi: no actor")
	}
	raw, err := json.Marshal(act)
	if err != nil {
		return wire.ServerFrame{}, err
	}
	ack := s.Actor.HandleVia(ctx, p, ViaMCP, "mcp-"+runrequest.NewID(), room, wire.ClientFrame{Type: wire.FrameAct, ClientSeq: 1, Action: raw})
	if ack.Rejected != "" {
		return ack, toolError("rejected: " + ack.Rejected)
	}
	return ack, nil
}
