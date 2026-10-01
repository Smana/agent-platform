// SPDX-License-Identifier: Apache-2.0

package bridgeapi

import (
	"encoding/json"
	"slices"

	"github.com/Smana/agent-platform/internal/envelope"
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
	}
	return "", nil, false
}
