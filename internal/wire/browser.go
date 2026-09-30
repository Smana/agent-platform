// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"encoding/json"

	"github.com/Smana/agent-platform/internal/envelope"
)

// The browser's frame types (Appendix B): one JSON object per WebSocket text frame.
const (
	FrameHello     = "hello"
	FrameAct       = "act"
	FramePing      = "ping"
	FrameState     = "state"
	FrameSync      = "sync"
	FrameEvent     = "event"
	FrameAck       = "ack"
	FrameTransient = "transient"
)

// ClientFrame is a frame from a browser or roomctl: hello first, then acts and pings.
type ClientFrame struct {
	Type        string          `json:"type"`
	RoomID      string          `json:"roomId,omitempty"`
	AfterSeq    *int64          `json:"afterSeq,omitempty"`
	Tail        int             `json:"tail,omitempty"`
	ClientSeq   int64           `json:"clientSeq,omitempty"`
	Action      json.RawMessage `json:"action,omitempty"`
	DriverEpoch *int64          `json:"driverEpoch,omitempty"`
}

// RunView is one run of the room, as the snapshot shows it.
type RunView struct {
	ID    string `json:"id"`
	Role  string `json:"role"`
	Phase string `json:"phase"`
}

// You is the viewer's own standing in the room (§1).
type You struct {
	Principal string `json:"principal"`
	Role      string `json:"role"`
	Approver  bool   `json:"approver"`
	Driver    bool   `json:"driver"`
	WebUI     bool   `json:"webUI"`
}

// Snapshot is the room's state at the high-water mark of a state frame.
type Snapshot struct {
	RoomID      string    `json:"roomId"`
	Phase       string    `json:"phase"`
	Driver      string    `json:"driver"`
	DriverEpoch int64     `json:"driverEpoch"`
	DataClass   string    `json:"dataClass"`
	You         You       `json:"you"`
	Runs        []RunView `json:"runs"`
}

// ServerFrame is a frame from the broker: state, sync, event, ack or transient.
type ServerFrame struct {
	Type       string          `json:"type"`
	ThroughSeq int64           `json:"throughSeq,omitempty"`
	FromSeq    int64           `json:"fromSeq,omitempty"`
	Snapshot   *Snapshot       `json:"snapshot,omitempty"`
	Event      *envelope.Event `json:"event,omitempty"`
	ClientSeq  int64           `json:"clientSeq,omitempty"`
	Seq        int64           `json:"seq,omitempty"`
	Rejected   string          `json:"rejected,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"` // e.g. a rendered AgentRun before SP3
}
