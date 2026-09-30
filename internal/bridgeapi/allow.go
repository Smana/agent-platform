// SPDX-License-Identifier: Apache-2.0

package bridgeapi

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"unicode"

	"github.com/Smana/agent-platform/internal/envelope"
	"github.com/Smana/agent-platform/internal/wire"
)

// bridgeKinds are the state_changed kinds a bridge produces: its status tracker and
// mapping (phase 1), its acks (phase 4) and its local decisions (phase 5).
var bridgeKinds = map[string]bool{"harness_status": true, "harness_error": true, "harness_paused": true,
	"harness_event": true, "delivered": true, "interrupted": true, "policy_decision": true, "decision_applied": true}

// knownFields are, per type a bridge may push, the top-level keys a reader of
// the log looks up. A key that folds onto one of them must be spelled as it.
//
// state_changed has no envelope struct, so its list is every field
// docs/event-envelope.md documents for a state_changed kind. Any new reader of
// a state_changed field must add that field here, or give its kind a struct:
// otherwise a bridge can store a variant spelling that Go readers see and jsonb
// readers do not.
var knownFields = map[envelope.Type][]string{
	envelope.Message:    jsonNames[envelope.MessagePayload](),
	envelope.Turn:       jsonNames[envelope.TurnPayload](),
	envelope.ToolCall:   jsonNames[envelope.ToolCallPayload](),
	envelope.ToolResult: jsonNames[envelope.ToolResultPayload](),
	envelope.StateChanged: {"kind", "phase", "owner", "driver", "dataClass", "reason", "events", "bytes",
		"running", "status", "previous", "code", "detail", "harnessKind", "url", "verdictSeq", "runId",
		"ref", "callId", "class", "decision", "room", "seq", "note"},
}

// jsonNames lists a struct's JSON keys.
func jsonNames[T any]() []string {
	t := reflect.TypeFor[T]()
	out := make([]string, 0, t.NumField())
	for f := range t.Fields() {
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name != "" && name != "-" {
			out = append(out, name)
		}
	}
	return out
}

// bridgePayload checks a redacted payload against what a room bridge may push
// (review M5): what its own code produces, never a verdict, a driver change or a
// decision it could forge. It returns the payload to store, or a refusal reason.
//
// Every check reads the redacted payload, the one stored. Each key must have
// one spelling (Ruling AI): a message is re-marshalled from its envelope
// struct, and every other payload is refused if two of its keys fold together
// or a key folds to a known field without being spelled as it.
func bridgePayload(t envelope.Type, redacted json.RawMessage) (json.RawMessage, string) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(redacted, &obj); err != nil || obj == nil {
		return nil, wire.ReasonBadPayload
	}
	known, ok := knownFields[t]
	if !ok || !singleSpelling(obj, known) {
		return nil, wire.ReasonBadItem
	}
	switch t {
	case envelope.Turn, envelope.ToolCall, envelope.ToolResult:
		return redacted, ""
	case envelope.Message:
		var m envelope.MessagePayload
		if decodeStrict(bytes.NewReader(redacted), &m) != nil || m.Kind != envelope.KindChat ||
			(m.Delivery != envelope.DeliveryNone && m.Delivery != "") {
			return nil, wire.ReasonBadItem
		}
		// A chat never carries a verdict's fields, whatever the bridge sent.
		m.Delivery, m.Verdict, m.Commit, m.PullRequest = envelope.DeliveryNone, "", "", ""
		return envelope.Must(m), ""
	case envelope.StateChanged:
		var kind string
		if json.Unmarshal(obj["kind"], &kind) != nil || !bridgeKinds[kind] {
			return nil, wire.ReasonBadItem
		}
		return redacted, ""
	}
	return nil, wire.ReasonBadItem
}

// singleSpelling reports that no two keys of obj fold together as encoding/json
// folds them, and that a key folding to a known name is spelled exactly as it.
// Otherwise a Go reader, which folds, and a jsonb reader, which does not, would
// see different values under one name.
func singleSpelling(obj map[string]json.RawMessage, known []string) bool {
	folded := make(map[string]bool, len(obj))
	for k := range obj {
		f := foldKey(k)
		if folded[f] {
			return false
		}
		folded[f] = true
	}
	for _, name := range known {
		if _, exact := obj[name]; !exact && folded[foldKey(name)] {
			return false
		}
	}
	return true
}

// foldKey is encoding/json's foldName: each rune replaced by the smallest rune
// of its simple case-folding orbit, so foldKey(a) == foldKey(b) exactly when
// strings.EqualFold(a, b). ſ (U+017F) folds with s, K (U+212A) with k.
func foldKey(k string) string {
	var b strings.Builder
	b.Grow(len(k))
	for _, r := range k {
		for {
			next := unicode.SimpleFold(r)
			if next <= r {
				r = next
				break
			}
			r = next
		}
		b.WriteRune(r)
	}
	return b.String()
}
