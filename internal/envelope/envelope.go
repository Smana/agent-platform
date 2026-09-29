// Package envelope is the C4 event envelope, frozen at v1 (programme C4). SP2 owns it.
package envelope

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
)

const (
	Version         = 1
	MaxPayload      = 64 << 10 // C4
	MaxToolOutput   = 16 << 10 // SP2 §4
	MaxHumanMessage = 16 << 10 // SP2 §4
)

var idRE = regexp.MustCompile(`^[a-z2-7]{8}$`)

// ValidID reports whether s is a C2 id: 8 characters of lowercase unpadded base32.
func ValidID(s string) bool { return idRE.MatchString(s) }

type ActorKind string

const (
	ActorAgent  ActorKind = "agent"
	ActorHuman  ActorKind = "human"
	ActorSystem ActorKind = "system"
)

// Actor is stamped by the broker from the authenticated credential, never taken from a client.
type Actor struct {
	Kind ActorKind `json:"kind"`
	ID   string    `json:"id"`
	Role string    `json:"role,omitempty"`
}

type Type string

const (
	Message           Type = "message"
	Turn              Type = "turn"
	ToolCall          Type = "tool_call"
	ToolResult        Type = "tool_result"
	ApprovalRequested Type = "approval_requested"
	ApprovalDecided   Type = "approval_decided"
	Participant       Type = "participant"
	Driver            Type = "driver"
	Handoff           Type = "handoff"
	StateChanged      Type = "state_changed"
)

var types = map[Type]bool{Message: true, Turn: true, ToolCall: true, ToolResult: true,
	ApprovalRequested: true, ApprovalDecided: true, Participant: true, Driver: true,
	Handoff: true, StateChanged: true}

func (t Type) Valid() bool { return types[t] }

type Origin string

const (
	OriginHarness Origin = "harness"
	OriginBroker  Origin = "broker"
	OriginClient  Origin = "client"
)

// Event is one durable entry of a room's log.
type Event struct {
	V          int             `json:"v"`
	ID         string          `json:"id"`
	Seq        int64           `json:"seq"`
	RoomID     string          `json:"roomId"`
	RunID      string          `json:"runId,omitempty"`
	Actor      Actor           `json:"actor"`
	Type       Type            `json:"type"`
	CausedBy   *int64          `json:"causedBy,omitempty"`
	Origin     Origin          `json:"origin"`
	TS         time.Time       `json:"ts"`
	Redactions []string        `json:"redactions"`
	Payload    json.RawMessage `json:"payload"`
}

// Draft is everything a writer hands the store. The store stamps v, id, seq and ts.
// OriginClient and OriginSeq are the idempotency key: "agent:<runId>" + harnessSeq,
// "human:<sub>" + clientSeq, or "broker:<what>" + a fixed step number.
type Draft struct {
	RoomID       string
	RunID        string
	Actor        Actor
	Type         Type
	CausedBy     *int64
	Origin       Origin
	OriginClient string
	OriginSeq    int64
	Redactions   []string
	Payload      json.RawMessage
}

func (d Draft) Validate() error {
	switch {
	case !ValidID(d.RoomID):
		return fmt.Errorf("roomId %q is not a C2 id", d.RoomID)
	case d.RunID != "" && !ValidID(d.RunID):
		return fmt.Errorf("runId %q is not a C2 id", d.RunID)
	case !d.Type.Valid():
		return fmt.Errorf("unknown event type %q", d.Type)
	case d.Actor.ID == "" || (d.Actor.Kind != ActorAgent && d.Actor.Kind != ActorHuman && d.Actor.Kind != ActorSystem):
		return errors.New("the actor is stamped by the broker and must be complete")
	case d.Origin != OriginHarness && d.Origin != OriginBroker && d.Origin != OriginClient:
		return fmt.Errorf("unknown origin %q", d.Origin)
	case d.OriginClient == "":
		return errors.New("originClient is the idempotency scope and is required")
	case len(d.Payload) == 0 || !json.Valid(d.Payload):
		return errors.New("payload must be a JSON document")
	}
	return nil
}

// Must marshals a payload struct; the structs here cannot fail to marshal.
func Must(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// StatePayload builds a state_changed payload: {kind, …fields}.
func StatePayload(kind string, fields map[string]any) json.RawMessage {
	m := map[string]any{"kind": kind}
	for k, v := range fields {
		m[k] = v
	}
	return Must(m)
}

// Oversize is stored in place of a payload over MaxPayload (ruling P20): refusing a
// harness event would block the bridge's cursor forever.
func Oversize(t Type, n int) json.RawMessage {
	return Must(map[string]any{"oversize": true, "bytes": n, "type": t})
}
