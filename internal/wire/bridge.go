// SPDX-License-Identifier: Apache-2.0

// Package wire holds the frames of Appendix B: the types both ends of a
// connection share, so the bridge and the broker cannot drift apart.
package wire

import (
	"encoding/json"
	"time"

	"github.com/Smana/agent-platform/internal/envelope"
)

// Stream separates the two idempotency scopes of a run: its harness events
// (agent:<runId>) and the status transitions the bridge synthesises (agent:<runId>:status).
type Stream string

// The two streams of a run.
const (
	StreamEvents Stream = "events"
	StreamStatus Stream = "status"
)

// ApprovalPolicy is the room's approval profile, handed to the bridge at hello
// (phase 5; empty until then).
type ApprovalPolicy struct {
	Profile   string            `json:"profile"`
	Overrides map[string]string `json:"overrides,omitempty"`
	TTL       string            `json:"ttl,omitempty"`
}

// Resume answers the bridge's hello: where the log already is.
type Resume struct {
	RoomID          string         `json:"roomId"`
	AfterHarnessSeq int64          `json:"afterHarnessSeq"`
	AfterStatusSeq  int64          `json:"afterStatusSeq"`
	Approvals       ApprovalPolicy `json:"approvals"`
}

// Item is one event a bridge pushes. Seq is its idempotency key within Stream.
type Item struct {
	Stream  Stream          `json:"stream"`
	Seq     int64           `json:"seq"`
	Type    envelope.Type   `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// Batch is the body of POST /v1/bridge/events.
type Batch struct {
	Items []Item `json:"items"`
}

// BatchAck is the highest seq of each stream the log now holds from the batch.
type BatchAck struct {
	AfterHarnessSeq int64 `json:"afterHarnessSeq"`
	AfterStatusSeq  int64 `json:"afterStatusSeq"`
}

// Error is the body of every refusal on :8443. Reason is one of the Reason
// constants: stable strings a client may branch on, unlike the status text.
type Error struct {
	Reason string `json:"error"`
}

// The refusal reasons of :8443. A bridge never drops its buffer on a 409: both
// room_busy and lease_lost mean another run holds the room (Ruling Y), and the
// bridge says hello again later.
const (
	ReasonUnauthenticated = "unauthenticated"   // 401: no valid token for this API
	ReasonNotPermitted    = "not_permitted"     // 403: a valid token outside the allowlist
	ReasonRunNotLive      = "run_not_live"      // 403: the run ended, was revoked, or is not watched
	ReasonRunHasNoRoom    = "run_has_no_room"   // 403: the run names no room
	ReasonRoomBusy        = "room_busy"         // 409 on hello: another live run holds the room
	ReasonLeaseLost       = "lease_lost"        // 409 on events: another run took the room's bridge lease
	ReasonSealed          = "sealed"            // 410: the room's log is sealed
	ReasonBadBatch        = "bad_batch"         // 400: the body is not a batch
	ReasonBatchTooLarge   = "batch_too_large"   // 413: over the byte or item cap; split it
	ReasonBadItem         = "bad_item"          // 400: an item a bridge may not push (review M5), or keys spelled two ways
	ReasonBadPayload      = "bad_payload"       // 400: a payload that is not a JSON object
	ReasonBadRoom         = "bad_room"          // 400: not a C2 room id
	ReasonBadMessage      = "bad_message"       // 400: not a task_state message, or not a message to queue
	ReasonBadStream       = "bad_stream"        // 400: a queue stream that is not [a-z]{1,16}
	ReasonBadConsume      = "bad_consume"       // 400: a consume without a C2 run id, or over 100 refs
	ReasonNoQueue         = "no_queue"          // 501: this broker serves no queue routes
	ReasonBadApproval     = "bad_approval"      // 400: not an approval request: no eventId or callId, or an unknown class
	ReasonBadAction       = "bad_action"        // 400: an approval's action that is not a JSON object, or over the cap
	ReasonNoRoom          = "no_room"           // 404 (system API) or 503 (hello, before the Room's first reconcile)
	ReasonRateLimited     = "rate_limited"      // 429: over the principal's request rate or requests in flight; retry after Retry-After
	ReasonLogUnavailable  = "log_unavailable"   // 503: the log could not be read or written; retry
	ReasonTimedOut        = "timed_out"         // 503: the request's deadline passed before it was written; retry
	ReasonStreamingFailed = "streaming_refused" // 500: the connection cannot stream
)

// SSE event names, broker → bridge.
const (
	EventDeliver   = "deliver"
	EventDecision  = "decision"
	EventInterrupt = "interrupt"
)

// Deliver hands a queued human message to the run (phase 4).
type Deliver struct {
	Ref  int64  `json:"ref"`
	Text string `json:"text"`
}

// Decision answers an approval the harness is waiting on (phase 5). Ref is the
// approval_decided event's seq, which the bridge acknowledges as decision_applied.
type Decision struct {
	ApprovalID string `json:"approvalId"`
	Allow      bool   `json:"allow"`
	Reason     string `json:"reason,omitempty"`
	Ref        int64  `json:"ref"`
}

// ApprovalRequest is the body of POST /v1/bridge/approvals: a pending action
// whose class the room's policy sends to its approvers (phase 5). EventID, the
// harness event's id, keys the request's idempotency: a model provider may
// reuse a tool call id (review I2).
type ApprovalRequest struct {
	EventID string          `json:"eventId"`
	CallID  string          `json:"callId"`
	Class   string          `json:"class"`
	Action  json.RawMessage `json:"action"`
}

// ApprovalAck answers an ApprovalRequest: the approval's id, which a later
// decision names, and when the broker expires it.
type ApprovalAck struct {
	ApprovalID string    `json:"approvalId"`
	ExpiresAt  time.Time `json:"expiresAt"`
}

// Interrupt stops the run's current turn (phase 4).
type Interrupt struct {
	Ref int64 `json:"ref"`
}
