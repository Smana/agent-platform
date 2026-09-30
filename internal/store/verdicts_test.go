// SPDX-License-Identifier: Apache-2.0

package store

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Smana/agent-platform/internal/envelope"
)

var reviewer = envelope.Actor{Kind: envelope.ActorAgent, ID: "agent:7f3cq2xz", Role: "reviewer"}

func verdictDraft(n int64, actor envelope.Actor) envelope.Draft {
	return envelope.Draft{RoomID: room, RunID: "7f3cq2xz", Actor: actor, Type: envelope.Message,
		Origin: envelope.OriginClient, OriginClient: actor.ID + ":tools", OriginSeq: n,
		Payload: envelope.Must(envelope.MessagePayload{Kind: envelope.KindReviewVerdict, Text: "Looks right.", Verdict: "approve",
			Commit: "4be1c9d", Delivery: envelope.DeliveryNone, PullRequest: "https://github.com/Smana/cloud-native-ref/pull/12"})}
}

func TestUnpostedVerdicts(t *testing.T) {
	s, _, _, _ := open(t)
	ctx := t.Context()
	if _, _, err := s.Append(ctx, draft("agent:7f3cq2xz", 1)); err != nil { // a chat message is never a verdict
		t.Fatal(err)
	}
	agent, _, err := s.Append(ctx, verdictDraft(1, reviewer))
	if err != nil {
		t.Fatal(err)
	}
	// A verdict pushed as a harness event is a forgery: only room_verdict writes one (review M5).
	forged := verdictDraft(3, reviewer)
	forged.Origin, forged.OriginClient = envelope.OriginHarness, "agent:7f3cq2xz"
	if _, _, err := s.Append(ctx, forged); err != nil {
		t.Fatal(err)
	}
	// Client-origin but not the room tools' scope: not room_verdict's either.
	elsewhere := verdictDraft(4, reviewer)
	elsewhere.OriginClient = "agent:7f3cq2xz"
	if _, _, err := s.Append(ctx, elsewhere); err != nil {
		t.Fatal(err)
	}
	// An implementer holds no room_verdict.
	if _, _, err := s.Append(ctx, verdictDraft(5, envelope.Actor{Kind: envelope.ActorAgent, ID: "agent:7f3cq2xz", Role: "implementer"})); err != nil {
		t.Fatal(err)
	}
	// Each filter alone: every other column looks like room_verdict's.
	harness := verdictDraft(6, reviewer)
	harness.Origin = envelope.OriginHarness
	humanInScope := verdictDraft(7, envelope.Actor{Kind: envelope.ActorHuman, ID: "human:ana", Role: "reviewer"})
	humanInScope.OriginClient = "agent:7f3cq2xz:tools"
	chat := verdictDraft(8, reviewer)
	chat.Payload = envelope.Must(envelope.MessagePayload{Kind: envelope.KindChat, Text: "lgtm", Delivery: envelope.DeliveryNone})
	for _, d := range []envelope.Draft{harness, humanInScope, chat} {
		if _, _, err := s.Append(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	// A human's verdict is theirs to post on GitHub.
	if _, _, err := s.Append(ctx, verdictDraft(2, envelope.Actor{Kind: envelope.ActorHuman, ID: "human:ana"})); err != nil {
		t.Fatal(err)
	}
	got, err := s.UnpostedVerdicts(ctx, time.Now().Add(-time.Hour), 10)
	if err != nil || len(got) != 1 || got[0].Seq != agent.Seq || got[0].RoomID != room {
		t.Fatalf("got %+v, err %v", got, err)
	}
	var p envelope.MessagePayload
	if err := json.Unmarshal(got[0].Payload, &p); err != nil || p.PullRequest == "" || got[0].Actor != reviewer {
		t.Fatalf("the whole event is returned: %+v %+v", got[0], p)
	}
	if got, _ := s.UnpostedVerdicts(ctx, time.Now().Add(time.Minute), 10); len(got) != 0 {
		t.Fatal("outside the window")
	}
	if _, _, err := s.Append(ctx, envelope.Draft{RoomID: room, Actor: envelope.Actor{Kind: envelope.ActorSystem, ID: "system:room-broker"},
		Type: envelope.StateChanged, Origin: envelope.OriginBroker, OriginClient: VerdictsClient, OriginSeq: agent.Seq,
		Payload: envelope.StatePayload("verdict_posted", map[string]any{"verdictSeq": agent.Seq})}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.UnpostedVerdicts(ctx, time.Now().Add(-time.Hour), 10); len(got) != 0 {
		t.Fatalf("recorded, still returned: %+v", got)
	}
}

func TestUnpostedVerdictsOldestFirstAndLimited(t *testing.T) {
	s, _, _, _ := open(t)
	ctx := t.Context()
	var seqs []int64
	for n := range int64(3) {
		ev, _, err := s.Append(ctx, verdictDraft(n+1, reviewer))
		if err != nil {
			t.Fatal(err)
		}
		seqs = append(seqs, ev.Seq)
	}
	got, err := s.UnpostedVerdicts(ctx, time.Now().Add(-time.Hour), 2)
	if err != nil || len(got) != 2 || got[0].Seq != seqs[0] || got[1].Seq != seqs[1] {
		t.Fatalf("got %+v, err %v", got, err)
	}
}

// A sealed room can record no outcome, so retrying its verdict would call GitHub forever.
func TestASealedRoomsVerdictIsSkipped(t *testing.T) {
	s, _, _, _ := open(t)
	ctx := t.Context()
	if _, _, err := s.Append(ctx, verdictDraft(1, reviewer)); err != nil {
		t.Fatal(err)
	}
	if err := s.CloseRoom(ctx, room, "deleted"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.UnpostedVerdicts(ctx, time.Now().Add(-time.Hour), 10); len(got) != 0 {
		t.Fatalf("got %+v", got)
	}
}

// One posted verdict suppresses only itself: its room's other verdict is still
// listed (review 3.4, mutant verdicts-any-origin-seq).
func TestAPostedVerdictHidesOnlyItself(t *testing.T) {
	s, _, _, _ := open(t)
	ctx := t.Context()
	first, _, err := s.Append(ctx, verdictDraft(1, reviewer))
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := s.Append(ctx, verdictDraft(2, reviewer))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Append(ctx, envelope.Draft{RoomID: room, Actor: envelope.Actor{Kind: envelope.ActorSystem, ID: "system:room-broker"},
		Type: envelope.StateChanged, Origin: envelope.OriginBroker, OriginClient: VerdictsClient, OriginSeq: first.Seq,
		Payload: envelope.StatePayload("verdict_posted", map[string]any{"verdictSeq": first.Seq})}); err != nil {
		t.Fatal(err)
	}
	got, err := s.UnpostedVerdicts(ctx, time.Now().Add(-time.Hour), 10)
	if err != nil || len(got) != 1 || got[0].Seq != second.Seq {
		t.Fatalf("got %+v, err %v", got, err)
	}
}

// since is exclusive: a verdict stamped exactly then is outside the window.
func TestUnpostedVerdictsSinceIsExclusive(t *testing.T) {
	s, _, _, _ := open(t)
	ctx := t.Context()
	v, _, err := s.Append(ctx, verdictDraft(1, reviewer))
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.UnpostedVerdicts(ctx, v.TS, 10); err != nil || len(got) != 0 {
		t.Fatalf("at the verdict's ts: %+v, %v", got, err)
	}
	if got, err := s.UnpostedVerdicts(ctx, v.TS.Add(-time.Microsecond), 10); err != nil || len(got) != 1 {
		t.Fatalf("a microsecond before: %+v, %v", got, err)
	}
}

// The index is built CONCURRENTLY, outside a transaction (review 3.4 m1), and
// must come out valid: a failed concurrent build leaves an invalid one behind.
func TestTheVerdictIndexIsValid(t *testing.T) {
	_, _, _, super := testDB(t)
	conn, err := pgx.Connect(t.Context(), super)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(t.Context()) }()
	var valid bool
	if err := conn.QueryRow(t.Context(), `SELECT i.indisvalid FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
		WHERE c.relname = 'events_agent_verdicts'`).Scan(&valid); err != nil || !valid {
		t.Fatalf("valid %v, err %v", valid, err)
	}
	if b, err := os.ReadFile("migrations/20260929120000_verdicts.sql"); err != nil || !strings.HasPrefix(string(b), "-- atlas:txmode none\n") {
		t.Fatal("Atlas must run the migration outside a transaction: CONCURRENTLY refuses one")
	}
}
