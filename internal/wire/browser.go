// SPDX-License-Identifier: Apache-2.0

package wire

import (
	"encoding/json"
	"time"

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

// QueuedView is one message still queued for the next run.
type QueuedView struct {
	Ref    int64  `json:"ref"`
	Author string `json:"author"`
	Text   string `json:"text"`
}

// ApprovalView is one approval still pending: its card (§6). Action is the raw,
// redacted call, never agent prose (T3).
type ApprovalView struct {
	ApprovalID string          `json:"approvalId"`
	RunID      string          `json:"runId"`
	CallID     string          `json:"callId"`
	Class      string          `json:"class"`
	Action     json.RawMessage `json:"action"`
	ExpiresAt  time.Time       `json:"expiresAt"`
	Seq        int64           `json:"seq"` // its approval_requested event
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
	// Queue is read after the mark: the client applies events past ThroughSeq over
	// it, so a row queued or moved in between is idempotent either way.
	Queue []QueuedView `json:"queue"`
	// Sealed is the log's own seal at the mark; Phase follows the Room CR and lags it.
	Sealed bool `json:"sealed"`
	// Approvals are the pending ones, read after the mark like Queue: a request
	// may be far behind the page's tail.
	Approvals []ApprovalView `json:"approvals"`
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
