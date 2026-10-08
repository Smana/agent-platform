// SPDX-License-Identifier: Apache-2.0

package envelope

import (
	"encoding/json"
	"errors"
	"regexp"
	"time"
)

// MessageKind distinguishes chat from SP3's reserved kinds.
type MessageKind string

// The message kinds.
const (
	KindChat          MessageKind = "chat"
	KindProgress      MessageKind = "progress"
	KindReviewVerdict MessageKind = "review_verdict" // SP3's reserved kind
	KindTaskState     MessageKind = "task_state"     // SP3's reserved kind
)

// Delivery is how a message reaches a running agent.
type Delivery string

// The delivery modes.
const (
	DeliveryNone     Delivery = "none"
	DeliveryQueued   Delivery = "queued"
	DeliverySteering Delivery = "steering"
)

// MessagePayload is a message event's payload.
type MessagePayload struct {
	Kind     MessageKind `json:"kind"`
	Text     string      `json:"text"`
	To       []string    `json:"to,omitempty"`
	Delivery Delivery    `json:"delivery"`
	Verdict  string      `json:"verdict,omitempty"` // approve | changes (review_verdict only)
	Commit   string      `json:"commit,omitempty"`
	// PullRequest is the pull request a review_verdict is about (SP2 design §3).
	// Additive: C4 v1's envelope is unchanged, and SP3 reads it (ruling P29).
	PullRequest string `json:"pullRequest,omitempty"`
}

// TurnPayload is a turn event's payload.
type TurnPayload struct {
	RunID  string `json:"runId"`
	TurnID string `json:"turnId"`
	Phase  string `json:"phase"` // started | completed | cancelled | failed
}

// ToolCallPayload is a tool_call event's payload.
type ToolCallPayload struct {
	CallID    string          `json:"callId"`
	Tool      string          `json:"tool"`
	Args      json.RawMessage `json:"args"`
	Class     string          `json:"class,omitempty"`
	Risk      string          `json:"risk,omitempty"`
	DecidedBy *string         `json:"decidedBy"` // policy | human | null
}

// ToolResultPayload is a tool_result event's payload.
type ToolResultPayload struct {
	CallID    string `json:"callId"`
	Status    string `json:"status"` // ok | error | rejected
	Output    string `json:"output"`
	Truncated bool   `json:"truncated"`
	Bytes     int    `json:"bytes"`
}

// ApprovalRequestedPayload is an approval_requested event's payload.
type ApprovalRequestedPayload struct {
	ApprovalID string          `json:"approvalId"`
	CallID     string          `json:"callId"`
	Class      string          `json:"class"`
	Action     json.RawMessage `json:"action"` // the raw, redacted call (T3)
	ExpiresAt  time.Time       `json:"expiresAt"`
}

// ApprovalDecidedPayload is an approval_decided event's payload.
type ApprovalDecidedPayload struct {
	ApprovalID string `json:"approvalId"`
	Decision   string `json:"decision"` // approved | denied | expired
	Reason     string `json:"reason,omitempty"`
}

// ParticipantPayload is a participant event's payload.
type ParticipantPayload struct {
	Principal string `json:"principal"`
	Change    string `json:"change"` // joined | left | role_changed
	Role      string `json:"role,omitempty"`
	Approver  bool   `json:"approver,omitempty"`
}

// DriverPayload is a driver event's payload: the driver token moved.
type DriverPayload struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Epoch  int64  `json:"epoch"`
	Reason string `json:"reason"` // given | requested | taken | lease_expired
}

// HandoffPayload is a handoff event's payload: work passed between roles.
type HandoffPayload struct {
	FromRole string `json:"fromRole"`
	ToRole   string `json:"toRole"`
	Summary  string `json:"summary"`
	Commit   string `json:"commit"`
	Branch   string `json:"branch"`
}

// MaxProgressNote bounds a progress note, in characters.
const MaxProgressNote = 280

// TaskFacts are what the factory knows about a room's task, written as state_changed{kind:task}
// so the room summary depends on the room log alone.
type TaskFacts struct {
	Phase  string      `json:"phase"`
	Reason string      `json:"reason,omitempty"`
	Run    *RunFact    `json:"run,omitempty"`
	Budget *BudgetFact `json:"budget,omitempty"`
	Issue  *IssueFact  `json:"issue,omitempty"`
	PR     *PRFact     `json:"pr,omitempty"`
}

// RunFact is a run's role, trigger, and lifecycle in a task.
type RunFact struct {
	ID        string    `json:"id"`
	Role      string    `json:"role"`
	Trigger   string    `json:"trigger,omitempty"`
	StartedAt time.Time `json:"startedAt,omitzero"`
}

// BudgetFact is the token budget and usage of a task.
type BudgetFact struct {
	UsedTokens  int64 `json:"usedTokens"`
	LimitTokens int64 `json:"limitTokens"`
}

// IssueFact is a GitHub issue linked to a task.
type IssueFact struct {
	Number     int    `json:"number"`
	URL        string `json:"url"`
	Author     string `json:"author,omitempty"`
	LabelledBy string `json:"labelledBy,omitempty"`
}

// PRFact is a GitHub pull request linked to a task.
type PRFact struct {
	Number    int      `json:"number"`
	URL       string   `json:"url"`
	Author    string   `json:"author,omitempty"`
	Reviewers []string `json:"reviewers,omitempty"`
}

var githubURL = regexp.MustCompile(`^https://github\.com/[A-Za-z0-9-]+/[A-Za-z0-9._-]+/(issues|pull)/[0-9]+$`)

// Validate refuses facts a broker should not store.
func (f TaskFacts) Validate() error {
	switch {
	case f.Phase == "":
		return errors.New("task facts: a phase is required")
	case f.Budget != nil && (f.Budget.UsedTokens < 0 || f.Budget.LimitTokens < 0):
		return errors.New("task facts: token counts are not negative")
	case f.Issue != nil && !githubURL.MatchString(f.Issue.URL):
		return errors.New("task facts: the issue URL is a github.com issue")
	case f.PR != nil && !githubURL.MatchString(f.PR.URL):
		return errors.New("task facts: the PR URL is a github.com pull request")
	}
	return nil
}

// TaskStatePayload is the state_changed payload of a TaskFacts: {"kind":"task", ...facts}.
func TaskStatePayload(f TaskFacts) json.RawMessage {
	b, _ := json.Marshal(f) // a struct of strings, ints and slices: Marshal cannot fail
	var fields map[string]any
	_ = json.Unmarshal(b, &fields)
	return StatePayload("task", fields)
}
