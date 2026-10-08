// SPDX-License-Identifier: Apache-2.0

package store

import (
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Smana/agent-platform/internal/envelope"
)

func agentDraft(n int64, typ envelope.Type, payload any) envelope.Draft {
	return envelope.Draft{RoomID: room, RunID: "7f3cq2xz",
		Actor: envelope.Actor{Kind: envelope.ActorAgent, ID: "agent:7f3cq2xz", Role: "implementer"},
		Type:  typ, Origin: envelope.OriginClient, OriginClient: "agent:7f3cq2xz:tools", OriginSeq: n, Payload: envelope.Must(payload)}
}

// The brief quotes only the latest agent handoff and the latest agent review
// verdict (review 4.4 M3, I3): a human's message, a later chat, a tool result
// and an older handoff are never among its sources.
func TestBriefSources(t *testing.T) {
	ctx := t.Context()
	s, _, _, _ := open(t)
	appendOK := func(d envelope.Draft) int64 {
		t.Helper()
		ev, _, err := s.Append(ctx, d)
		if err != nil {
			t.Fatal(err)
		}
		return ev.Seq
	}
	if got, err := s.BriefSources(ctx, room); err != nil || len(got) != 0 {
		t.Fatalf("an empty room: %+v, %v", got, err)
	}
	appendOK(agentDraft(1, envelope.Handoff, envelope.HandoffPayload{FromRole: "implementer", ToRole: "reviewer", Summary: "old", Commit: "1111111"}))
	verdict := appendOK(agentDraft(2, envelope.Message, envelope.MessagePayload{Kind: envelope.KindReviewVerdict, Verdict: "changes", Text: "fix it"}))
	handoff := appendOK(agentDraft(3, envelope.Handoff, envelope.HandoffPayload{FromRole: "implementer", ToRole: "reviewer", Summary: "new", Commit: "2222222"}))
	appendOK(agentDraft(4, envelope.Message, envelope.MessagePayload{Kind: envelope.KindChat, Text: "later chat"}))
	appendOK(agentDraft(5, envelope.ToolResult, map[string]any{"text": "https://github.com/x/y/pull/9"}))
	human := envelope.Draft{RoomID: room, Actor: envelope.Actor{Kind: envelope.ActorHuman, ID: "human:own"}, Type: envelope.Message,
		Origin: envelope.OriginClient, OriginClient: "human:own:s1", OriginSeq: 1,
		Payload: envelope.Must(envelope.MessagePayload{Kind: envelope.KindChat, Text: "human", Delivery: envelope.DeliveryNone})}
	appendOK(human)
	got, err := s.BriefSources(ctx, room)
	if err != nil {
		t.Fatal(err)
	}
	var seqs []int64
	for _, ev := range got {
		seqs = append(seqs, ev.Seq)
	}
	if !slices.Equal(seqs, []int64{verdict, handoff}) || got[1].Actor.Kind != envelope.ActorAgent || !strings.Contains(string(got[1].Payload), `"new"`) {
		t.Fatalf("sources %v, want [verdict %d, handoff %d]: %+v", seqs, verdict, handoff, got)
	}
	// A fork reads them as they stood at its seq: the commit at the fork point (§5).
	got, err = s.BriefSourcesThrough(ctx, room, verdict)
	if err != nil || len(got) != 2 || got[0].Seq != 1 || got[1].Seq != verdict {
		t.Fatalf("through seq %d: %+v, %v", verdict, got, err)
	}
}

// A requested run is pending until it joins, and for 10 minutes at most (review 4.4 I2).
func TestPendingRuns(t *testing.T) {
	ctx := t.Context()
	s, _, _, _ := open(t)
	if _, err := s.RecordRunRequest(ctx, runDraft(1), "aaaaaaaa", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordRunRequest(ctx, runDraft(2), "bbbbbbbb", nil, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := s.PendingRuns(ctx, room, 10*time.Minute); err != nil || !slices.Equal(got, []string{"aaaaaaaa", "bbbbbbbb"}) {
		t.Fatalf("%v, %v", got, err)
	}
	joined := envelope.Draft{RoomID: room, Actor: envelope.Actor{Kind: envelope.ActorSystem, ID: "system:room-broker"}, Type: envelope.Participant,
		Origin: envelope.OriginBroker, OriginClient: "broker:run:aaaaaaaa", OriginSeq: 1,
		Payload: envelope.Must(envelope.ParticipantPayload{Principal: "agent:aaaaaaaa", Change: "joined", Role: "implementer"})}
	if _, _, err := s.Append(ctx, joined); err != nil {
		t.Fatal(err)
	}
	if got, err := s.PendingRuns(ctx, room, 10*time.Minute); err != nil || !slices.Equal(got, []string{"bbbbbbbb"}) {
		t.Fatalf("a joined run is not pending: %v, %v", got, err)
	}
	// Past the TTL a request no longer holds the room: its claim was never applied.
	s.Now = func() time.Time { return time.Now().Add(-11 * time.Minute) }
	if _, err := s.RecordRunRequest(ctx, runDraft(3), "cccccccc", nil, nil); err != nil {
		t.Fatal(err)
	}
	if got, err := s.PendingRuns(ctx, room, 10*time.Minute); err != nil || !slices.Equal(got, []string{"bbbbbbbb"}) {
		t.Fatalf("an expired request is not pending: %v, %v", got, err)
	}
}

// Both reads go through events_brief, never a scan of the room's log.
func TestBriefReadsUseTheirIndex(t *testing.T) {
	s, _, _, super := open(t)
	forge(t, super, `INSERT INTO events (room_id, seq, id, run_id, actor_kind, actor_id, type, origin, origin_client,
		origin_seq, ts, payload)
		SELECT '`+room+`', g, 'id' || g, '7f3cq2xz', 'agent', 'agent:7f3cq2xz', 'message', 'harness', 'agent:7f3cq2xz', g, now(),
		'{"kind":"chat","text":"x","delivery":"none"}'
		FROM generate_series(1, 20000) g;
		UPDATE rooms SET last_seq = 20000 WHERE room_id = '`+room+`';
		ANALYZE events;`)
	for _, c := range []struct {
		name, sql string
		args      []any
	}{
		{"brief sources", briefSourcesSQL, []any{room, int64(math.MaxInt64)}},
		{"brief sources through a fork point", briefSourcesSQL, []any{room, int64(10000)}},
		{"pending runs", pendingRunsSQL, []any{room, 600.0}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if text := plan(t, s, c.sql, c.args...); !strings.Contains(text, "events_brief") || strings.Contains(text, "Seq Scan on events") {
				t.Fatalf("%s does not read through events_brief:\n%s", c.name, text)
			}
		})
	}
}
