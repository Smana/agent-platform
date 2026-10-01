// SPDX-License-Identifier: Apache-2.0

package bridgeapi

import (
	"encoding/json"
	"slices"

	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/store"
	"github.com/Smana/agent-platform/internal/wire"
)

// Deliverable derives what the running run must receive from the log itself:
// deliveries are durable, replica-agnostic, and resume from the bridge's last ack.
func Deliverable(ev envelope.Event, runID string) (string, []byte, bool) {
	switch ev.Type {
	case envelope.Message:
		var p envelope.MessagePayload
		if json.Unmarshal(ev.Payload, &p) == nil && p.Delivery == envelope.DeliverySteering && slices.Contains(p.To, "agent:"+runID) {
			b, _ := json.Marshal(wire.Deliver{Ref: ev.Seq, Text: p.Text})
			return wire.EventDeliver, b, true
		}
	case envelope.StateChanged:
		var p struct {
			Kind  string `json:"kind"`
			RunID string `json:"runId"`
		}
		if json.Unmarshal(ev.Payload, &p) == nil && p.Kind == "interrupt" && p.RunID == runID {
			b, _ := json.Marshal(wire.Interrupt{Ref: ev.Seq})
			return wire.EventInterrupt, b, true
		}
	case envelope.ApprovalDecided:
		// The bridge acts on approved, denied and expired (5.2 contracts 3 and 4); a
		// superseded approval's call already has its result, so nothing waits for it.
		var p envelope.ApprovalDecidedPayload
		if ev.RunID == runID && json.Unmarshal(ev.Payload, &p) == nil && p.ApprovalID != "" {
			d := wire.Decision{ApprovalID: p.ApprovalID, Allow: p.Decision == store.ApprovalApproved, Reason: p.Reason, Ref: ev.Seq}
			switch p.Decision {
			case store.ApprovalExpired:
				d.Reason = "expired" // the bridge tells the agent no one decided in time
				fallthrough
			case store.ApprovalApproved, store.ApprovalDenied:
				b, _ := json.Marshal(d)
				return wire.EventDecision, b, true
			}
		}
	}
	return "", nil, false
}
