// SPDX-License-Identifier: Apache-2.0

package bridge

import (
	"encoding/json"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/Smana/agent-platform/internal/envelope"
)

// ItemsPerEvent is how many seqs each harness event owns: event i (1-based) owns
// 4i … 4i+3, so a resumed bridge re-maps from event afterHarnessSeq/4 and the
// store's idempotency drops what already landed. Map yields at most two.
const ItemsPerEvent = 4

// SeqFor is the idempotency key of item k (0-based) of harness event i (1-based).
func SeqFor(eventIndex int64, k int) int64 { return eventIndex*ItemsPerEvent + int64(k) }

// Mapped is one C4 item a harness event becomes, ready for the bridge to push.
// Its payload is always a JSON object within envelope.MaxPayload.
type Mapped struct {
	Type    envelope.Type
	Payload json.RawMessage
}

const (
	// maxStateField caps each string of a state_changed payload. Two fields at
	// most, each escaping to at most six bytes a byte, stay under MaxPayload.
	maxStateField = 4 << 10
	// truncatedMark ends a message cut to fit MaxPayload.
	truncatedMark = "\n[truncated]"
)

// dropped never enter the log (§3 Event mapping). Streaming deltas are transient
// and not forwarded at all (ruling P4); state updates are polled (status.go).
var dropped = map[string]bool{"SystemPromptEvent": true, "LLMCompletionLogEvent": true,
	"Condensation": true, "CondensationRequest": true, "CondensationSummaryEvent": true,
	"TokenEvent": true, "StreamingDeltaEvent": true, "ConversationStateUpdateEvent": true,
	"HookExecutionEvent": true}

type textPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func texts(parts []textPart) string {
	var b strings.Builder
	for _, p := range parts {
		if p.Type == "text" {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// cut keeps at most n bytes of s without splitting a rune. It backs off at most
// three bytes, so invalid UTF-8 cannot make it keep nothing.
func cut(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for i := 0; i < utf8.UTFMax-1 && n > 0 && !utf8.RuneStart(s[n]); i++ {
		n--
	}
	return s[:n]
}

// fitted replaces a payload over MaxPayload with the store's own oversize stub
// (ruling P20). Only for the types whose stub the broker accepts: tool_call,
// tool_result and turn. Pushing the whole payload instead could pass the
// broker's 2 MiB batch cap and block the cursor.
func fitted(t envelope.Type, p json.RawMessage) Mapped {
	if len(p) > envelope.MaxPayload {
		p = envelope.Oversize(t, len(p))
	}
	return Mapped{t, p}
}

// fitText is build(s) when that fits MaxPayload, else build of the longest
// prefix of s, cut at a rune boundary, whose marshalled payload fits. Measuring
// the marshalled payload matters: a control byte, <, > or & escapes to six
// bytes, so a raw-byte estimate over-cuts. When not even the empty prefix fits
// (a huge callId), it is the oversize stub.
func fitText(t envelope.Type, s string, build func(kept string) json.RawMessage) Mapped {
	full := build(s)
	if len(full) <= envelope.MaxPayload {
		return Mapped{t, full}
	}
	fits := func(n int) bool { return len(build(cut(s, n))) <= envelope.MaxPayload }
	// Each kept byte is at least one byte of JSON, so no prefix past MaxPayload
	// fits: the search marshals about 17 payloads of bounded size.
	n := sort.Search(min(len(s), envelope.MaxPayload)+1, func(n int) bool { return !fits(n) })
	if n == 0 {
		return fitted(t, full)
	}
	return Mapped{t, build(cut(s, n-1))}
}

// chat is a message item. The broker decodes a message strictly, so an oversize
// stub would be refused: the text is cut to fit instead, and says so.
func chat(text string, to []string) Mapped {
	return fitText(envelope.Message, text, func(kept string) json.RawMessage {
		if len(kept) < len(text) {
			kept += truncatedMark
		}
		return envelope.Must(envelope.MessagePayload{Kind: envelope.KindChat, Text: kept, To: to, Delivery: envelope.DeliveryNone})
	})
}

// state is a state_changed item. The broker refuses one without its kind, so its
// strings are capped rather than stubbed.
func state(kind string, fields map[string]string) Mapped {
	m := make(map[string]any, len(fields))
	for k, v := range fields {
		m[k] = cut(v, maxStateField)
	}
	return Mapped{envelope.StateChanged, envelope.StatePayload(kind, m)}
}

// Map turns one harness event into zero, one or two C4 items, following the spec
// table (§3 Event mapping). It is total: fields of the wrong type read as empty,
// a kind agent-server 1.49.6 did not have becomes a harness_event state_changed
// that records the kind and nothing the event said, and a Malformed event one
// that records only "malformed".
func Map(e RawEvent, runID string) []Mapped {
	if e.Malformed {
		// Still one item: the room shows that the harness wrote something here.
		return []Mapped{state("harness_event", map[string]string{"harnessKind": "malformed"})}
	}
	if dropped[e.Kind] {
		return nil
	}
	var f struct {
		LLMMessage struct {
			Content []textPart `json:"content"`
		} `json:"llm_message"`
		Thought      []textPart      `json:"thought"`
		ToolName     string          `json:"tool_name"`
		ToolCallID   string          `json:"tool_call_id"`
		Action       json.RawMessage `json:"action"`
		SecurityRisk string          `json:"security_risk"`
		Observation  struct {
			Content []textPart `json:"content"`
			// Not in the brief's fixtures: verify against 1.49.6's Observation.
			// Without it an error observation maps to ok, a degradation only.
			IsError bool `json:"is_error"`
		} `json:"observation"`
		RejectionReason string `json:"rejection_reason"`
		Error           string `json:"error"`
		Code            string `json:"code"`
		Detail          string `json:"detail"`
	}
	// A type mismatch leaves that field empty and the rest decoded.
	_ = json.Unmarshal(e.Raw, &f)
	result := func(status, out string) Mapped {
		return fitText(envelope.ToolResult, cut(out, envelope.MaxToolOutput), func(kept string) json.RawMessage {
			return envelope.Must(envelope.ToolResultPayload{CallID: f.ToolCallID, Status: status, Output: kept,
				Truncated: len(kept) < len(out), Bytes: len(out)})
		})
	}
	switch e.Kind {
	case "MessageEvent":
		var to []string
		if e.Source == "user" {
			to = []string{"agent:" + runID} // the task, or an injected message: addressed to the agent
		}
		return []Mapped{chat(texts(f.LLMMessage.Content), to)}
	case "ActionEvent":
		args := f.Action
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}
		call := fitted(envelope.ToolCall, envelope.Must(envelope.ToolCallPayload{CallID: f.ToolCallID,
			Tool: f.ToolName, Args: args, Risk: f.SecurityRisk}))
		if t := texts(f.Thought); t != "" {
			return []Mapped{chat(t, nil), call}
		}
		return []Mapped{call}
	case "ObservationEvent":
		status := "ok"
		if f.Observation.IsError {
			status = "error"
		}
		return []Mapped{result(status, texts(f.Observation.Content))}
	case "UserRejectObservation":
		return []Mapped{result("rejected", f.RejectionReason)}
	case "AgentErrorEvent":
		return []Mapped{result("error", f.Error)}
	case "ConversationErrorEvent":
		return []Mapped{state("harness_error", map[string]string{"code": f.Code, "detail": f.Detail})}
	case "PauseEvent":
		return []Mapped{state("harness_paused", nil)}
	case "InterruptEvent":
		return []Mapped{fitted(envelope.Turn, envelope.Must(envelope.TurnPayload{RunID: runID, TurnID: e.ID, Phase: "cancelled"}))}
	default:
		return []Mapped{state("harness_event", map[string]string{"harnessKind": e.Kind})}
	}
}
