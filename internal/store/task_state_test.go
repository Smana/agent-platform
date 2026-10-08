// SPDX-License-Identifier: Apache-2.0

package store

import (
	"strings"
	"testing"

	"github.com/Smana/agent-platform/internal/envelope"
)

func TestLastTaskStateIsTheHighestSeq(t *testing.T) {
	s, _, _, _ := open(t)
	ctx := t.Context()
	if p, err := s.LastTaskState(ctx, room); err != nil || p != nil {
		t.Fatalf("no task event: %s, %v", p, err)
	}
	for i, payload := range []envelope.Draft{
		stateDraft(1, envelope.StatePayload("task", map[string]any{"phase": "Planning"})),
		stateDraft(2, envelope.StatePayload("task", map[string]any{"phase": "Merged"})),
		stateDraft(3, envelope.StatePayload("run_phase", map[string]any{"phase": "Failed"})),
	} {
		if _, _, err := s.Append(ctx, payload); err != nil {
			t.Fatal(i, err)
		}
	}
	p, err := s.LastTaskState(ctx, room)
	if err != nil || !strings.Contains(string(p), `"Merged"`) {
		t.Fatalf("payload = %s, %v: want the latest task event, not the later run_phase", p, err)
	}
}

func stateDraft(n int64, payload []byte) envelope.Draft {
	d := draft("factory", n)
	d.Type, d.Origin, d.Payload = envelope.StateChanged, envelope.OriginBroker, payload
	d.Actor = envelope.Actor{Kind: envelope.ActorSystem, ID: "system:factory"}
	d.RunID = ""
	return d
}
