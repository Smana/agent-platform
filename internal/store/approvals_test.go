// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/Smana/agent-platform/internal/envelope"
)

const approvalRun = "7f3cq2xz"

func approvalDraft() envelope.Draft {
	return envelope.Draft{RoomID: room, RunID: approvalRun, Actor: envelope.Actor{Kind: envelope.ActorAgent, ID: "agent:" + approvalRun},
		Origin: envelope.OriginHarness, OriginClient: "agent:" + approvalRun + ":approvals", OriginSeq: time.Now().UnixNano(), Payload: []byte(`{}`)}
}

// holdBridge gives run the room's bridge lease, as its hello does: approval
// requests are fenced on it.
func holdBridge(t *testing.T, s *Store, roomID, run string) {
	t.Helper()
	if _, took, err := s.ClaimBridge(t.Context(), roomID, run, time.Minute, func(context.Context, string) bool { return false }); err != nil || !took {
		t.Fatalf("claim the bridge: %v %v", took, err)
	}
}

func approval(id, eventID, callID string, expires time.Duration) Approval {
	return Approval{ID: id, RoomID: room, RunID: approvalRun, EventID: eventID, CallID: callID, Class: "forge.pr",
		Action: []byte(`{"command":"gh pr create"}`), Prompters: []string{"human:own"}, ExpiresAt: time.Now().Add(expires)}
}

func deciderDraft(who string, n int64) envelope.Draft {
	return envelope.Draft{Actor: envelope.Actor{Kind: envelope.ActorHuman, ID: who},
		Origin: envelope.OriginClient, OriginClient: who + ":s", OriginSeq: n}
}

func TestFirstDecisionWins(t *testing.T) {
	s, _, _, _ := open(t)
	holdBridge(t, s, room, approvalRun)
	a, ev, err := s.RequestApproval(context.Background(), Approval{ID: "ap1", RoomID: room, RunID: approvalRun, EventID: "e1", CallID: "c1",
		Class: "forge.pr", Action: []byte(`{"command":"gh pr create"}`), ExpiresAt: time.Now().Add(30 * time.Minute)}, approvalDraft())
	if err != nil || ev.Type != envelope.ApprovalRequested || a.State != "pending" {
		t.Fatal(a, ev, err)
	}
	again, _, _ := s.RequestApproval(context.Background(), Approval{ID: "ap2", RoomID: room, RunID: approvalRun, EventID: "e1", CallID: "c1",
		Class: "forge.pr", Action: []byte(`{}`), ExpiresAt: time.Now().Add(time.Hour)}, approvalDraft())
	if again.ID != "ap1" {
		t.Fatal("a re-sent request is the same approval")
	}
	var wg sync.WaitGroup
	results := make([]error, 2)
	for i, decision := range []string{"approved", "denied"} {
		wg.Add(1)
		go func(i int, decision string) {
			defer wg.Done()
			_, _, results[i] = s.Decide(context.Background(), "ap1", decision, "human:"+decision, "", deciderDraft("human:"+decision, 1))
		}(i, decision)
	}
	wg.Wait()
	won, lost := 0, 0
	for _, e := range results {
		switch {
		case e == nil:
			won++
		case errors.Is(e, ErrAlreadyDecided):
			lost++
		}
	}
	if won != 1 || lost != 1 {
		t.Fatalf("results = %v", results)
	}
	if n, _ := s.PendingApprovals(context.Background(), room); n != 0 {
		t.Fatal("no longer pending")
	}
	// The expiry sweep after a decision closes nothing.
	if evs, err := s.ExpireDue(t.Context()); err != nil || len(evs) != 0 {
		t.Fatal(evs, err)
	}
}

func TestExpiry(t *testing.T) {
	s, _, _, _ := open(t)
	holdBridge(t, s, room, approvalRun)
	_, _, _ = s.RequestApproval(context.Background(), Approval{ID: "ap1", RoomID: room, RunID: approvalRun, EventID: "e1", CallID: "c1",
		Class: "forge.pr", Action: []byte(`{}`), ExpiresAt: time.Now().Add(-time.Second)}, approvalDraft())
	_, _, _ = s.RequestApproval(context.Background(), approval("ap2", "e2", "c2", time.Hour), approvalDraft())
	evs, err := s.ExpireDue(context.Background())
	if err != nil || len(evs) != 1 || string(evs[0].Payload) != `{"approvalId":"ap1","decision":"expired","reason":"no decision before the deadline"}` || evs[0].RunID != approvalRun {
		t.Fatal(evs, err)
	}
	if evs[0].Actor.ID != "system:room-broker" || evs[0].Origin != envelope.OriginBroker {
		t.Fatalf("the expiry is the broker's: %+v", evs[0])
	}
	if _, _, err := s.Decide(t.Context(), "ap1", "approved", "human:x", "", deciderDraft("human:x", 1)); !errors.Is(err, ErrAlreadyDecided) {
		t.Fatalf("a decision after the expiry: %v", err)
	}
	if again, err := s.ExpireDue(t.Context()); err != nil || len(again) != 0 {
		t.Fatalf("a second sweep closed %d, %v", len(again), err)
	}
	if a, err := s.Approval(t.Context(), "ap2"); err != nil || a.State != ApprovalPending {
		t.Fatalf("an approval before its deadline stays pending: %+v %v", a, err)
	}
}

// 5.2 contract 1: one approval per (room, run, eventId), stored, so it survives a
// broker restart; never per callId, which a model provider may reuse.
func TestRequestApprovalIsIdempotentPerEvent(t *testing.T) {
	ctx := t.Context()
	s, broker, _, _ := open(t)
	if _, _, err := s.RequestApproval(ctx, approval("ap1", "e1", "c1", time.Hour), approvalDraft()); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("without the bridge lease: %v", err)
	}
	holdBridge(t, s, room, approvalRun)
	first, ev, err := s.RequestApproval(ctx, approval("ap1", "e1", "c1", time.Hour), approvalDraft())
	if err != nil || first.RequestedSeq != ev.Seq || first.RequestedAt.IsZero() || ev.RunID != approvalRun {
		t.Fatal(first, ev, err)
	}
	var p envelope.ApprovalRequestedPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil || p.ApprovalID != "ap1" || p.CallID != "c1" || p.Class != "forge.pr" ||
		string(p.Action) != `{"command":"gh pr create"}` || !p.ExpiresAt.Equal(first.ExpiresAt) {
		t.Fatalf("payload %s, %v", ev.Payload, err)
	}
	restarted, err := Open(ctx, broker)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	again, none, err := restarted.RequestApproval(ctx, approval("apX", "e1", "c1", 2*time.Hour), approvalDraft())
	if err != nil || again.ID != "ap1" || none.Seq != 0 || !again.ExpiresAt.Equal(first.ExpiresAt) {
		t.Fatalf("after a restart: %+v %+v %v", again, none, err)
	}
	reused, _, err := s.RequestApproval(ctx, approval("ap2", "e2", "c1", time.Hour), approvalDraft())
	if err != nil || reused.ID != "ap2" {
		t.Fatalf("a reused call id in another event is another approval: %+v %v", reused, err)
	}
	if n, err := s.PendingApprovals(ctx, room); err != nil || n != 2 {
		t.Fatalf("pending = %d, %v", n, err)
	}
	other := approval("ap3", "e3", "c3", time.Hour)
	other.RunID = "aaaaaaaa"
	if _, _, err := s.RequestApproval(ctx, other, approvalDraft()); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("a run that does not hold the lease: %v", err)
	}
	huge := approval("ap4", "e4", "c4", time.Hour)
	huge.Action = envelope.Must(map[string]string{"x": strings.Repeat("y", envelope.MaxPayload)})
	if _, _, err := s.RequestApproval(ctx, huge, approvalDraft()); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("an oversize request: %v", err)
	}
	if _, _, err := s.RequestApproval(ctx, approval("ap5", "", "c5", time.Hour), approvalDraft()); err == nil {
		t.Fatal("a request with no event id")
	}
	if err := s.CloseRoom(ctx, room, "done"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RequestApproval(ctx, approval("ap6", "e6", "c6", time.Hour), approvalDraft()); !errors.Is(err, ErrSealed) {
		t.Fatalf("a sealed room: %v", err)
	}
	if n, err := s.PendingApprovals(ctx, room); err != nil || n != 0 {
		t.Fatalf("a sealed room has nothing to decide: %d, %v", n, err)
	}
	if _, _, err := s.Decide(ctx, "ap1", "approved", "human:x", "", deciderDraft("human:x", 1)); !errors.Is(err, ErrSealed) {
		t.Fatalf("a decision in a sealed room: %v", err)
	}
	if evs, err := s.ExpireDue(ctx); err != nil || len(evs) != 0 {
		t.Fatalf("a sealed room's approvals never expire: %v %v", evs, err)
	}
}

// Fenced like the driver's acts: a replayed key returns its decision, a key
// stored for anything else is a conflict, and only approved or denied is a
// human decision.
func TestDecideReplaysItsKey(t *testing.T) {
	ctx := t.Context()
	s, _, _, _ := open(t)
	holdBridge(t, s, room, approvalRun)
	for i, id := range []string{"ap1", "ap2"} {
		if _, _, err := s.RequestApproval(ctx, approval(id, fmt.Sprint("e", i), fmt.Sprint("c", i), time.Hour), approvalDraft()); err != nil {
			t.Fatal(err)
		}
	}
	ev, a, err := s.Decide(ctx, "ap1", "denied", "human:x", "not this branch", deciderDraft("human:x", 1))
	if err != nil || a.State != ApprovalDenied || ev.Actor.ID != "human:x" ||
		string(ev.Payload) != `{"approvalId":"ap1","decision":"denied","reason":"not this branch"}` {
		t.Fatal(ev, a, err)
	}
	replay, ra, err := s.Decide(ctx, "ap1", "denied", "human:x", "not this branch", deciderDraft("human:x", 1))
	if err != nil || replay.Seq != ev.Seq || ra.State != ApprovalDenied {
		t.Fatalf("a replayed decision: %+v %+v %v", replay, ra, err)
	}
	if _, _, err := s.Decide(ctx, "ap2", "approved", "human:x", "", deciderDraft("human:x", 1)); !errors.Is(err, ErrKeyConflict) {
		t.Fatalf("the key of a decision on another approval: %v", err)
	}
	msg := deciderDraft("human:x", 2)
	msg.RoomID, msg.Type, msg.Payload = room, envelope.Message, envelope.Must(envelope.MessagePayload{Kind: envelope.KindChat, Text: "hi", Delivery: envelope.DeliveryNone})
	if _, _, err := s.Append(ctx, msg); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Decide(ctx, "ap2", "approved", "human:x", "", deciderDraft("human:x", 2)); !errors.Is(err, ErrKeyConflict) {
		t.Fatalf("a message's key: %v", err)
	}
	for _, bad := range []string{"expired", "superseded", "pending", "Approved", ""} {
		if _, _, err := s.Decide(ctx, "ap2", bad, "human:x", "", deciderDraft("human:x", 3)); !errors.Is(err, ErrBadDecision) {
			t.Fatalf("decision %q: %v", bad, err)
		}
	}
	if _, _, err := s.Decide(ctx, "nope", "approved", "human:x", "", deciderDraft("human:x", 4)); !errors.Is(err, ErrNoApproval) {
		t.Fatalf("an unknown approval: %v", err)
	}
	if _, err := s.Approval(ctx, "nope"); !errors.Is(err, ErrNoApproval) {
		t.Fatalf("read an unknown approval: %v", err)
	}
	if _, a, err := s.Decide(ctx, "ap2", "approved", "human:y", "", deciderDraft("human:y", 1)); err != nil || a.State != ApprovalApproved {
		t.Fatalf("approve: %+v %v", a, err)
	}
	var decidedBy, reason *string
	if err := s.pool.QueryRow(ctx, `SELECT decided_by, reason FROM approvals WHERE approval_id = 'ap2'`).Scan(&decidedBy, &reason); err != nil ||
		decidedBy == nil || *decidedBy != "human:y" || reason != nil {
		t.Fatalf("decided_by %v reason %v (%v)", decidedBy, reason, err)
	}
}

// toolResult appends the run's result for callID, as its bridge does.
func toolResult(t *testing.T, s *Store, n int64, callID string) {
	t.Helper()
	d := draft("agent:"+approvalRun, n)
	d.Type, d.Payload = envelope.ToolResult, envelope.Must(envelope.ToolResultPayload{CallID: callID, Status: "rejected"})
	if _, _, err := s.AppendAsBridge(t.Context(), approvalRun, d); err != nil {
		t.Fatal(err)
	}
}

// 5.2 contract 5: once its call has a tool_result, an approval is superseded,
// never left for approvers; a result of a reused call id from before the
// request is another call's.
func TestAnAnsweredCallSupersedesItsApproval(t *testing.T) {
	ctx := t.Context()
	s, _, _, _ := open(t)
	holdBridge(t, s, room, approvalRun)
	toolResult(t, s, 1, "c1") // a reused call id's earlier result
	for i, id := range []string{"ap1", "ap2", "ap3"} {
		if _, _, err := s.RequestApproval(ctx, approval(id, fmt.Sprint("e", i), fmt.Sprint("c", i+1), time.Hour), approvalDraft()); err != nil {
			t.Fatal(err)
		}
	}
	if evs, err := s.SupersedeAnswered(ctx); err != nil || len(evs) != 0 {
		t.Fatalf("an earlier result supersedes nothing: %v %v", evs, err)
	}
	toolResult(t, s, 2, "c1")
	toolResult(t, s, 3, "c2")
	other := draft("agent:aaaaaaaa", 1) // another run's result for c3
	other.RunID, other.Type = "aaaaaaaa", envelope.ToolResult
	other.Payload = envelope.Must(envelope.ToolResultPayload{CallID: "c3", Status: "ok"})
	if _, _, err := s.Append(ctx, other); err != nil {
		t.Fatal(err)
	}
	// A decision on an answered call is too late: it closes the approval as superseded.
	if _, _, err := s.Decide(ctx, "ap2", "approved", "human:x", "", deciderDraft("human:x", 1)); !errors.Is(err, ErrAlreadyDecided) {
		t.Fatalf("a decision after the result: %v", err)
	}
	evs, err := s.SupersedeAnswered(ctx)
	if err != nil || len(evs) != 1 || string(evs[0].Payload) != `{"approvalId":"ap1","decision":"superseded","reason":"the call already has a result"}` {
		t.Fatalf("superseded: %v %v", evs, err)
	}
	states := map[string]string{}
	for _, id := range []string{"ap1", "ap2", "ap3"} {
		a, err := s.Approval(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		states[id] = a.State
	}
	if states["ap1"] != ApprovalSuperseded || states["ap2"] != ApprovalSuperseded || states["ap3"] != ApprovalPending {
		t.Fatalf("states %v", states)
	}
	open, err := s.OpenApprovals(ctx, room)
	if err != nil || len(open) != 1 || open[0].ID != "ap3" || open[0].CallID != "c3" || open[0].Class != "forge.pr" ||
		!slices.Equal(open[0].Prompters, []string{"human:own"}) {
		t.Fatalf("open approvals: %+v %v", open, err)
	}
}

// The age of the oldest pending approval, in open rooms only, on the database's clock.
func TestOldestPending(t *testing.T) {
	ctx := t.Context()
	s, _, _, super := open(t)
	if age, n, err := s.OldestPending(ctx); err != nil || age != 0 || n != 0 {
		t.Fatalf("none: %v %d %v", age, n, err)
	}
	holdBridge(t, s, room, approvalRun)
	for i, id := range []string{"ap1", "ap2"} {
		if _, _, err := s.RequestApproval(ctx, approval(id, fmt.Sprint("e", i), fmt.Sprint("c", i), time.Hour), approvalDraft()); err != nil {
			t.Fatal(err)
		}
	}
	forge(t, super, `UPDATE approvals SET requested_at = now() - interval '20 minutes' WHERE approval_id = 'ap1'`)
	if age, n, err := s.OldestPending(ctx); err != nil || n != 2 || age < 19*time.Minute || age > 21*time.Minute {
		t.Fatalf("oldest %v of %d (%v)", age, n, err)
	}
	if _, _, err := s.Decide(ctx, "ap1", "approved", "human:x", "", deciderDraft("human:x", 1)); err != nil {
		t.Fatal(err)
	}
	if age, n, err := s.OldestPending(ctx); err != nil || n != 1 || age > time.Minute {
		t.Fatalf("after a decision: %v of %d (%v)", age, n, err)
	}
	if err := s.CloseRoom(ctx, room, "done"); err != nil {
		t.Fatal(err)
	}
	if age, n, err := s.OldestPending(ctx); err != nil || n != 0 || age != 0 {
		t.Fatalf("a sealed room's approvals wait for nobody: %v of %d (%v)", age, n, err)
	}
}

// OD-16: the humans who prompted a run are whoever requested it, steered it, or
// wrote a queued message its brief consumed or that was promoted to it.
func TestPrompters(t *testing.T) {
	ctx := t.Context()
	s, _, _, _ := open(t)
	put := func(who string, n int64, typ envelope.Type, payload any) {
		t.Helper()
		d := deciderDraft(who, n)
		d.RoomID, d.Type, d.Payload = room, typ, envelope.Must(payload)
		if _, _, err := s.Append(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	steer := func(to string) envelope.MessagePayload {
		return envelope.MessagePayload{Kind: envelope.KindChat, Text: "x", To: []string{to}, Delivery: envelope.DeliverySteering}
	}
	put("human:steerer", 1, envelope.Message, steer("agent:"+approvalRun))
	put("human:elsewhere", 1, envelope.Message, steer("agent:aaaaaaaa"))
	put("human:chatter", 1, envelope.Message, envelope.MessagePayload{Kind: envelope.KindChat, Text: "x", Delivery: envelope.DeliveryNone})
	if _, err := s.Enqueue(ctx, queuedDraft(1, "do this next"), "human:alice", "do this next"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enqueue(ctx, queuedDraft(2, "and this later"), "human:alice", "and this later"); err != nil {
		t.Fatal(err)
	}
	var refs []int64
	q, _ := s.Queue(ctx, room)
	refs = append(refs, q[0].Ref)
	req := deciderDraft("human:owner", 1)
	req.RoomID = room
	if _, err := s.RecordRunRequest(ctx, req, approvalRun, refs, map[string]any{"role": "implementer"}); err != nil {
		t.Fatal(err)
	}
	got, err := s.Prompters(ctx, room, approvalRun)
	if want := []string{"human:alice", "human:owner", "human:steerer"}; err != nil || !slices.Equal(got, want) {
		t.Fatalf("prompters = %v, %v; want %v", got, err, want)
	}
	if got, err := s.Prompters(ctx, room, "bbbbbbbb"); err != nil || len(got) != 0 {
		t.Fatalf("a run nobody prompted: %v, %v", got, err)
	}
}

// 5.2 contracts 3 and 4: a decision of the run's own approval is a delivery, and
// its decision_applied moves LastAck; a superseded one is never waited for.
func TestDecisionsAreDeliveries(t *testing.T) {
	ctx := t.Context()
	s, _, _, _ := open(t)
	holdBridge(t, s, room, approvalRun)
	for i, id := range []string{"ap1", "ap2", "ap3"} {
		if _, _, err := s.RequestApproval(ctx, approval(id, fmt.Sprint("e", i), fmt.Sprint("c", i), time.Hour), approvalDraft()); err != nil {
			t.Fatal(err)
		}
	}
	decided, _, err := s.Decide(ctx, "ap1", "approved", "human:x", "", deciderDraft("human:x", 1))
	if err != nil {
		t.Fatal(err)
	}
	toolResult(t, s, 1, "c1")
	superseded, err := s.SupersedeAnswered(ctx)
	if err != nil || len(superseded) != 1 {
		t.Fatal(superseded, err)
	}
	forgeExpiry := `UPDATE approvals SET expires_at = now() - interval '1 second' WHERE approval_id = 'ap3'`
	if _, err := s.pool.Exec(ctx, forgeExpiry); err == nil {
		t.Fatal("the broker moved a deadline")
	}
	st, _ := s.Room(ctx, room)
	got, err := s.Deliveries(ctx, room, approvalRun, 0, st.LastSeq, 10)
	if err != nil || len(got) != 1 || got[0].Seq != decided.Seq {
		t.Fatalf("deliveries = %v, %v; want the approved decision only", got, err)
	}
	if other, err := s.Deliveries(ctx, room, "aaaaaaaa", 0, st.LastSeq, 10); err != nil || len(other) != 0 {
		t.Fatalf("another run's deliveries: %v %v", other, err)
	}
	ack := func(n int64, ref int64) {
		t.Helper()
		d := draft("agent:"+approvalRun+":status", n)
		d.Type, d.Payload = envelope.StateChanged, envelope.StatePayload("decision_applied", map[string]any{"ref": ref, "runId": approvalRun})
		if _, _, err := s.AppendAsBridge(ctx, approvalRun, d); err != nil {
			t.Fatal(err)
		}
	}
	ack(1, superseded[0].Seq) // not a delivery: skipped
	if n, err := s.LastAck(ctx, room, approvalRun); err != nil || n != 0 {
		t.Fatalf("an ack of a superseded decision: %d, %v", n, err)
	}
	ack(2, decided.Seq)
	if n, err := s.LastAck(ctx, room, approvalRun); err != nil || n != decided.Seq {
		t.Fatalf("last ack = %d, %v; want %d", n, err, decided.Seq)
	}
}

// T12: the database holds an approval to its events, even against the broker's
// own credential: no row without its request, no rewrite, one close, on the record.
func TestApprovalsAreTheirEvents(t *testing.T) {
	ctx := t.Context()
	s, _, _, _ := open(t)
	holdBridge(t, s, room, approvalRun)
	if _, _, err := s.RequestApproval(ctx, approval("ap1", "e1", "c1", time.Hour), approvalDraft()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.RequestApproval(ctx, approval("ap2", "e2", "c2", time.Hour), approvalDraft()); err != nil {
		t.Fatal(err)
	}
	const (
		row = `INSERT INTO approvals (approval_id, room_id, run_id, event_id, call_id, class, action, state, requested_seq, expires_at) VALUES `
		req = `(SELECT requested_seq FROM approvals WHERE approval_id = 'ap1')`
		// A legitimate close by hand: the move, then its event.
		closeAp1 = `UPDATE approvals SET state = 'denied', decided_by = 'human:x', decided_at = now() WHERE approval_id = 'ap1';
			UPDATE rooms SET last_seq = last_seq + 1 WHERE room_id = '3kq7x2ma';
			INSERT INTO events (room_id, seq, id, run_id, actor_kind, actor_id, type, origin, origin_client, origin_seq, ts, payload)
			SELECT room_id, last_seq, 'hand', '7f3cq2xz', 'human', 'human:x', 'approval_decided', 'client', 'test:hand', 1, now(),
			'{"approvalId":"ap1","decision":"%s"}' FROM rooms WHERE room_id = '3kq7x2ma'`
	)
	for _, tc := range []struct {
		name, sql, code, msg string
	}{
		{"plant an approval with no request", row + `('apX', '3kq7x2ma', '7f3cq2xz', 'eX', 'c1', 'forge.pr', '{}', 'pending', 1, now())`, "23514", "not its approval_requested"},
		{"plant a copy of a request", row + `('apX', '3kq7x2ma', '7f3cq2xz', 'eX', 'c1', 'forge.pr', '{"command":"gh pr create"}', 'pending', ` + req + `, now())`, "23514", "not its approval_requested"},
		{"reword a request's action", row + `('ap1', '3kq7x2ma', '7f3cq2xz', 'eX', 'c1', 'forge.pr', '{"command":"ls"}', 'pending', ` + req + `, now())`, "23514", "not its approval_requested"},
		{"enter approved", row + `('apX', '3kq7x2ma', '7f3cq2xz', 'eX', 'c1', 'forge.pr', '{}', 'approved', 1, now())`, "23514", "enters pending"},
		{"backdate a request", `INSERT INTO approvals (approval_id, room_id, run_id, event_id, call_id, class, action, state, requested_seq, requested_at, expires_at)
			VALUES ('apX', '3kq7x2ma', '7f3cq2xz', 'eX', 'c1', 'forge.pr', '{}', 'pending', 1, now() - interval '1 day', now())`, "23514", "enters pending"},
		{"rewrite an action", `UPDATE approvals SET action = '{}'`, "42501", ""},
		{"rewrite the prompters", `UPDATE approvals SET prompters = '{}'`, "42501", ""},
		{"move a deadline", `UPDATE approvals SET expires_at = now()`, "42501", ""},
		{"delete an approval", `DELETE FROM approvals`, "42501", ""},
		{"close with no event", `UPDATE approvals SET state = 'approved', decided_by = 'human:x', decided_at = now() WHERE approval_id = 'ap1'`, "23514", "no approval_decided"},
		{"close behind another outcome's event", fmt.Sprintf(closeAp1, "approved"), "23514", "no approval_decided"},
		{"close nameless", `UPDATE approvals SET state = 'approved', decided_at = now() WHERE approval_id = 'ap1'`, "23514", "by someone"},
		{"close backdated", `UPDATE approvals SET state = 'approved', decided_by = 'human:x', decided_at = now() - interval '1 day' WHERE approval_id = 'ap1'`, "23514", "by someone"},
		{"re-open a pending approval", `UPDATE approvals SET state = 'pending', decided_by = 'human:x', decided_at = now() WHERE approval_id = 'ap1'`, "23514", "moves once"},
		{"close with its event by hand", fmt.Sprintf(closeAp1, "denied"), "", ""},
		{"close it again", `UPDATE approvals SET state = 'approved', decided_by = 'human:x', decided_at = now() WHERE approval_id = 'ap1'`, "23514", "moves once"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := inTx(ctx, s, tc.sql)
			if tc.code == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			var pg *pgconn.PgError
			if !errors.As(err, &pg) || pg.Code != tc.code || !strings.Contains(pg.Message, tc.msg) {
				t.Fatalf("want SQLSTATE %s mentioning %q, got %v", tc.code, tc.msg, err)
			}
		})
	}
	if err := s.CloseRoom(ctx, room, "done"); err != nil {
		t.Fatal(err)
	}
	err := inTx(ctx, s, `UPDATE approvals SET state = 'expired', decided_by = 'x', decided_at = now() WHERE approval_id = 'ap2'`)
	if pg, ok := errors.AsType[*pgconn.PgError](err); !ok || pg.Code != "23514" || !strings.Contains(pg.Message, "approvals stay") {
		t.Fatalf("a sealed room's approval moved: %v", err)
	}
	if !gapless(t, s, room) {
		t.Fatal("the log has a gap")
	}
}
