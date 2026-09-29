package envelope

import (
	"encoding/json"
	"time"
)

type MessageKind string

const (
	KindChat          MessageKind = "chat"
	KindReviewVerdict MessageKind = "review_verdict" // SP3's reserved kind
	KindTaskState     MessageKind = "task_state"     // SP3's reserved kind
)

type Delivery string

const (
	DeliveryNone     Delivery = "none"
	DeliveryQueued   Delivery = "queued"
	DeliverySteering Delivery = "steering"
)

type MessagePayload struct {
	Kind     MessageKind `json:"kind"`
	Text     string      `json:"text"`
	To       []string    `json:"to,omitempty"`
	Delivery Delivery    `json:"delivery"`
	Verdict  string      `json:"verdict,omitempty"` // approve | changes (review_verdict only)
	Commit   string      `json:"commit,omitempty"`
}

type TurnPayload struct {
	RunID  string `json:"runId"`
	TurnID string `json:"turnId"`
	Phase  string `json:"phase"` // started | completed | cancelled | failed
}

type ToolCallPayload struct {
	CallID    string          `json:"callId"`
	Tool      string          `json:"tool"`
	Args      json.RawMessage `json:"args"`
	Class     string          `json:"class,omitempty"`
	Risk      string          `json:"risk,omitempty"`
	DecidedBy *string         `json:"decidedBy"` // policy | human | null
}

type ToolResultPayload struct {
	CallID    string `json:"callId"`
	Status    string `json:"status"` // ok | error | rejected
	Output    string `json:"output"`
	Truncated bool   `json:"truncated"`
	Bytes     int    `json:"bytes"`
}

type ApprovalRequestedPayload struct {
	ApprovalID string          `json:"approvalId"`
	CallID     string          `json:"callId"`
	Class      string          `json:"class"`
	Action     json.RawMessage `json:"action"` // the raw, redacted call (T3)
	ExpiresAt  time.Time       `json:"expiresAt"`
}

type ApprovalDecidedPayload struct {
	ApprovalID string `json:"approvalId"`
	Decision   string `json:"decision"` // approved | denied | expired
	Reason     string `json:"reason,omitempty"`
}

type ParticipantPayload struct {
	Principal string `json:"principal"`
	Change    string `json:"change"` // joined | left | role_changed
	Role      string `json:"role,omitempty"`
	Approver  bool   `json:"approver,omitempty"`
}

type DriverPayload struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Epoch  int64  `json:"epoch"`
	Reason string `json:"reason"` // given | requested | taken | lease_expired
}

type HandoffPayload struct {
	FromRole string `json:"fromRole"`
	ToRole   string `json:"toRole"`
	Summary  string `json:"summary"`
	Commit   string `json:"commit"`
	Branch   string `json:"branch"`
}
