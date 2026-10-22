// SPDX-License-Identifier: Apache-2.0

package store

import (
	"testing"

	"github.com/Smana/agent-platform/internal/envelope"
)

// Loop policy part 2: the run's last harness_end, read the way runwatch reads its harness_status.
func TestLastHarnessEnd(t *testing.T) {
	s, _, _, _ := open(t)
	put := func(n int64, kind string, fields map[string]any) {
		t.Helper()
		if _, _, err := s.Append(t.Context(), envelope.Draft{RoomID: room, RunID: "7f3cq2xz",
			Actor: envelope.Actor{Kind: envelope.ActorAgent, ID: "agent:7f3cq2xz", Role: "implementer"},
			Type:  envelope.StateChanged, Origin: envelope.OriginHarness, OriginClient: "bridge:7f3cq2xz", OriginSeq: n,
			Payload: envelope.StatePayload(kind, fields)}); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := s.LastHarnessEnd(t.Context(), room, "7f3cq2xz"); err != nil || got != "" {
		t.Fatalf("no end yet: %q, %v", got, err)
	}
	put(1, envelope.HarnessEnd, map[string]any{"reason": "not_met"})
	put(2, envelope.HarnessEnd, map[string]any{"reason": "stalled"})
	put(3, "harness_status", map[string]any{"status": "paused", "previous": "running"})
	if got, err := s.LastHarnessEnd(t.Context(), room, "7f3cq2xz"); err != nil || got != "stalled" {
		t.Fatalf("got %q, %v: want the last, stalled", got, err)
	}
	if got, err := s.LastHarnessEnd(t.Context(), room, "zzzzzzzz"); err != nil || got != "" {
		t.Fatalf("another run: %q, %v", got, err)
	}
}
