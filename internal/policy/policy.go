// SPDX-License-Identifier: Apache-2.0

// Package policy is SP2 §1's authorization matrix: the one enforcement point (T7).
// Allowed decides an action for a Subject; Groups admits a human and Resolves their
// standing in one room. The group names are config, never constants here (Seams).
package policy

import (
	"slices"

	"github.com/Smana/agent-platform/api/v1alpha1"
	"github.com/Smana/agent-platform/internal/authn"
	"github.com/Smana/agent-platform/internal/envelope"
)

// policyEngine is the broker's approval-policy principal, the only system caller that
// decides an approval (S10).
const policyEngine = "system:policy"

// Groups names the identity provider's two agent groups, taken from config.
type Groups struct {
	Admin  string // owner and approver in every room
	Member string // watches every room; more where a room grants it
}

// Role is a human's cumulative standing in a room: None < Watcher < Collaborator < Owner.
type Role int

// The roles, in increasing order of what they allow.
const (
	None Role = iota
	Watcher
	Collaborator
	Owner
)

// ParseRole maps a Room member's role to a Role; anything unknown is None.
func ParseRole(s string) Role {
	switch s {
	case "watcher":
		return Watcher
	case "collaborator":
		return Collaborator
	case "owner":
		return Owner
	}
	return None
}

// Subject is who acts, with their standing in one room.
type Subject struct {
	Kind     envelope.ActorKind
	ID       string
	Role     Role
	Approver bool // the room's approver flag
	Driver   bool // holds the driver token
	WebUI    bool // came through the web client, not a CLI token (ruling P18)
}

// Action is one thing a Subject may do in a room.
type Action string

// The §1 actions.
const (
	Read          Action = "read"
	Chat          Action = "chat"
	Queue         Action = "queue"
	Steer         Action = "steer"
	Interrupt     Action = "interrupt"
	StartRun      Action = "start_run"
	Decide        Action = "decide"
	DriverRequest Action = "driver_request"
	DriverGive    Action = "driver_give"
	DriverTake    Action = "driver_take"
	Fork          Action = "fork"
	Invite        Action = "invite"
	Close         Action = "close"
	PromoteQueued Action = "promote_queued"
	RemoveQueued  Action = "remove_queued" // the author check is the caller's
)

// uiOnly is never allowed from a CLI token, which a local agent can drive (§8, ruling P18).
func uiOnly(a Action) bool {
	switch a {
	case Steer, Interrupt, Decide, DriverRequest, DriverGive, DriverTake, PromoteQueued:
		return true
	}
	return false
}

// Allowed reports whether s may perform a in the room s was resolved for.
func Allowed(s Subject, a Action) bool {
	switch s.Kind {
	case envelope.ActorAgent:
		return a == Read || a == Chat // room_read, room_post; handoff and verdict are gated per tool
	case envelope.ActorSystem:
		switch a {
		case Decide:
			return s.ID == policyEngine // never an agent, never the factory (S10)
		case Steer, Interrupt, PromoteQueued, DriverGive:
			// A give from a non-holder is a take, and promoting a queued message
			// is steering (review I1).
			return s.Driver
		case DriverRequest, DriverTake:
			return false // a system holder gives, and yields to humans
		default:
			return true
		}
	case envelope.ActorHuman:
	default:
		return false
	}
	if uiOnly(a) && !s.WebUI {
		return false
	}
	switch a {
	case Read, Fork:
		return s.Role >= Watcher || s.Driver
	case Chat, Queue, RemoveQueued:
		return s.Role >= Collaborator || s.Driver
	case Steer, Interrupt, PromoteQueued, DriverGive:
		return s.Driver
	case StartRun:
		return s.Driver || s.Role == Owner
	case Decide:
		return s.Approver || s.Role == Owner
	case DriverRequest:
		return s.Role >= Collaborator && !s.Driver
	case DriverTake, Invite, Close:
		return s.Role == Owner
	}
	return false
}

// in reports whether p carries the group name; an unset name never matches.
func in(p authn.Principal, name string) bool {
	return name != "" && slices.Contains(p.Groups, name)
}

// Admitted reports whether p may use the human API at all: it is in one of the two groups.
func (g Groups) Admitted(p authn.Principal) bool {
	return in(p, g.Admin) || in(p, g.Member)
}

// IsAdmin reports whether p is a human in the admin group, who sees every room whatever its
// repository's permissions say (D7).
func (g Groups) IsAdmin(p authn.Principal) bool {
	return p.Kind == envelope.ActorHuman && in(p, g.Admin)
}

// Resolve derives p's standing in one room from its groups and the Room's spec. driver
// is the current driver-token holder; webUI is whether p came through the web client.
func (g Groups) Resolve(room *v1alpha1.Room, p authn.Principal, driver string, webUI bool) Subject {
	s := Subject{Kind: p.Kind, ID: p.ID, Driver: p.ID == driver, WebUI: webUI}
	if p.Kind != envelope.ActorHuman {
		return s
	}
	if in(p, g.Member) {
		s.Role = Watcher
	}
	if room.Spec.Owner == p.ID {
		s.Role = Owner
	}
	for _, m := range room.Spec.Members {
		if m.Principal == p.ID {
			s.Role = max(s.Role, ParseRole(m.Role))
			s.Approver = s.Approver || m.Approver
		}
	}
	if in(p, g.Admin) {
		s.Role, s.Approver = Owner, true
	}
	return s
}
