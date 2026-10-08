// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Smana/agent-platform/internal/envelope"
)

var (
	// ErrAlreadyDecided is a decision on an approval that is no longer pending:
	// the first valid decision won, or it expired, or its call already has a result.
	ErrAlreadyDecided = errors.New("already_decided")
	// ErrNoApproval is an approval id the room log does not hold.
	ErrNoApproval = errors.New("no such approval")
	// ErrBadDecision is a human decision other than approved or denied.
	ErrBadDecision = errors.New("a decision is approved or denied")
	// ErrTooLarge is an approval whose request event would exceed the payload cap:
	// stored as an oversize stub, it would not be the request the row records.
	ErrTooLarge = errors.New("the approval request exceeds the payload cap")
)

// An approval's states: pending, then closed once (SP2 §6).
const (
	ApprovalPending    = "pending"
	ApprovalApproved   = "approved"
	ApprovalDenied     = "denied"
	ApprovalExpired    = "expired"
	ApprovalSuperseded = "superseded"
)

// The reasons the broker closes an approval with.
const (
	reasonExpired    = "no decision before the deadline"
	reasonSuperseded = "the call already has a result"
	reasonRunEnded   = "the run ended"
)

// Approval is one row of the approvals projection.
type Approval struct {
	ID, RoomID, RunID, EventID, CallID, Class, State string
	Action                                           json.RawMessage
	Prompters                                        []string
	RequestedSeq                                     int64
	RequestedAt, ExpiresAt                           time.Time
}

const approvalCols = `approval_id, room_id, run_id, event_id, call_id, class, state, action, prompters, requested_seq,
	requested_at, expires_at`

func scanApproval(row pgx.Row) (Approval, error) {
	var a Approval
	var action []byte
	err := row.Scan(&a.ID, &a.RoomID, &a.RunID, &a.EventID, &a.CallID, &a.Class, &a.State, &action, &a.Prompters,
		&a.RequestedSeq, &a.RequestedAt, &a.ExpiresAt)
	a.Action = action
	return a, err
}

// answeredSQL is an approval a's call with a tool_result after its request: the
// harness ran or rejected it, so nobody decides it any more (5.2 contract 5).
// After the request only: a reused call id's earlier result answers nothing.
// It reads through events_tool_results.
const answeredSQL = `EXISTS (SELECT 1 FROM events e WHERE e.room_id = a.room_id AND e.run_id = a.run_id
	AND e.type = 'tool_result' AND e.payload->>'callId' = a.call_id AND e.seq > a.requested_seq)`

// endedSQL is an approval a whose run has left the room: the broker's own
// participant{left} (runwatch.Events), appended once the run is terminal. Nothing
// will apply a decision, so nobody is asked any more. Before or after the request
// alike: a request that raced the run's end is just as dead. It reads through
// events_broker_scopes.
const endedSQL = `EXISTS (SELECT 1 FROM events e WHERE e.room_id = a.room_id AND e.origin = 'broker'
	AND e.origin_client = 'broker:run:' || a.run_id AND e.type = 'participant' AND e.payload->>'change' = 'left')`

// RequestApproval records a pending approval and appends approval_requested in
// one transaction, fenced on the run's bridge lease like its other appends
// (Ruling Y). It is idempotent per (room, run, eventId): a bridge that re-sends
// the request, after a restart too, gets the stored approval back and no event.
func (s *Store) RequestApproval(ctx context.Context, a Approval, d envelope.Draft) (Approval, envelope.Event, error) {
	got, ev, err := s.requestApproval(ctx, a, d)
	if err != nil {
		return Approval{}, envelope.Event{}, fmt.Errorf("store: request approval in room %s: %w", a.RoomID, err)
	}
	return got, ev, nil
}

func (s *Store) requestApproval(ctx context.Context, a Approval, d envelope.Draft) (Approval, envelope.Event, error) {
	if a.ID == "" || a.EventID == "" || a.CallID == "" || a.Class == "" || !envelope.ValidID(a.RunID) {
		return Approval{}, envelope.Event{}, errors.New("an approval names its id, run, event, call and class")
	}
	if a.Prompters == nil {
		a.Prompters = []string{}
	}
	a.ExpiresAt = a.ExpiresAt.UTC().Truncate(time.Microsecond)
	d.RoomID, d.RunID, d.Type = a.RoomID, a.RunID, envelope.ApprovalRequested
	d.Payload = envelope.Must(envelope.ApprovalRequestedPayload{ApprovalID: a.ID, CallID: a.CallID, Class: a.Class,
		Action: a.Action, ExpiresAt: a.ExpiresAt})
	if err := d.Validate(); err != nil {
		return Approval{}, envelope.Event{}, err
	}
	if len(d.Payload) > envelope.MaxPayload {
		return Approval{}, envelope.Event{}, ErrTooLarge
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Approval{}, envelope.Event{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// The room's row lock serialises a request and its retry: the second finds the first's row.
	if _, _, err := lockRoom(ctx, tx, a.RoomID); err != nil {
		return Approval{}, envelope.Event{}, err
	}
	existing, err := scanApproval(tx.QueryRow(ctx, `SELECT `+approvalCols+` FROM approvals
		WHERE room_id = $1 AND run_id = $2 AND event_id = $3`, a.RoomID, a.RunID, a.EventID))
	if err == nil {
		return existing, envelope.Event{}, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Approval{}, envelope.Event{}, err
	}
	ev, dup, err := s.appendTx(ctx, tx, d, fence{bridgeRun: a.RunID})
	if err != nil {
		return Approval{}, envelope.Event{}, err
	}
	if dup { // the key holds another event: this request was never recorded
		return Approval{}, envelope.Event{}, ErrKeyConflict
	}
	if err := tx.QueryRow(ctx, `INSERT INTO approvals (approval_id, room_id, run_id, event_id, call_id, class, action,
		prompters, state, requested_seq, expires_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'pending',$9,$10) RETURNING requested_at`,
		a.ID, a.RoomID, a.RunID, a.EventID, a.CallID, a.Class, []byte(a.Action), a.Prompters, ev.Seq, a.ExpiresAt,
	).Scan(&a.RequestedAt); err != nil {
		return Approval{}, envelope.Event{}, err
	}
	a.State, a.RequestedSeq = ApprovalPending, ev.Seq
	return a, ev, tx.Commit(ctx)
}

// Decide records a human's decision, approved or denied, and appends
// approval_decided in one transaction. UPDATE … WHERE state = 'pending' under the
// room's row lock is the whole race (§2): the first valid decision wins, and every
// later one, an expiry or a supersede included, is ErrAlreadyDecided. A decision
// on a call that already has a result closes the approval as superseded and is
// ErrAlreadyDecided too. Like the driver's acts, d's key is checked first: a
// replayed decision returns its stored event, and a key stored for anything else
// is ErrKeyConflict.
func (s *Store) Decide(ctx context.Context, approvalID, decision, by, reason string, d envelope.Draft) (envelope.Event, Approval, error) {
	if decision != ApprovalApproved && decision != ApprovalDenied {
		return envelope.Event{}, Approval{}, fmt.Errorf("store: decide approval %s: %w", approvalID, ErrBadDecision)
	}
	ev, a, err := s.closeApproval(ctx, approvalID, decision, by, reason, d)
	if err != nil {
		return envelope.Event{}, a, fmt.Errorf("store: decide approval %s: %w", approvalID, err)
	}
	return ev, a, nil
}

// brokerClose is the draft of the broker's own close of an approval: one key per
// approval, so two leaders' sweeps write it once.
func brokerClose(approvalID string) envelope.Draft {
	return envelope.Draft{Actor: envelope.Actor{Kind: envelope.ActorSystem, ID: brokerActor}, Origin: envelope.OriginBroker,
		OriginClient: "broker:approval:" + approvalID, OriginSeq: 1}
}

// brokerActor closes approvals that expired or were superseded.
const brokerActor = "system:room-broker"

func (s *Store) closeApproval(ctx context.Context, id, state, by, reason string, d envelope.Draft) (envelope.Event, Approval, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return envelope.Event{}, Approval{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var roomID string
	if err := tx.QueryRow(ctx, `SELECT room_id FROM approvals WHERE approval_id = $1`, id).Scan(&roomID); errors.Is(err, pgx.ErrNoRows) {
		return envelope.Event{}, Approval{}, ErrNoApproval
	} else if err != nil {
		return envelope.Event{}, Approval{}, err
	}
	sealed, _, err := lockRoom(ctx, tx, roomID)
	if err != nil {
		return envelope.Event{}, Approval{}, err
	}
	d.RoomID = roomID
	// Before the seal and the state: the retry of a decision that won sees it won.
	if prev, dup, err := stored(ctx, tx, d); err != nil {
		return envelope.Event{}, Approval{}, err
	} else if dup {
		return replayedDecision(ctx, tx, prev, id)
	}
	if sealed { // its approvals stay as they are (approvals_are_their_events)
		return envelope.Event{}, Approval{}, ErrSealed
	}
	human := state == ApprovalApproved || state == ApprovalDenied
	if human {
		var answered, ended bool
		if err := tx.QueryRow(ctx, `SELECT `+answeredSQL+`, `+endedSQL+` FROM approvals a WHERE a.approval_id = $1 AND a.state = 'pending'`,
			id).Scan(&answered, &ended); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return envelope.Event{}, Approval{}, err
		}
		if answered || ended { // too late for anyone: close it for what it is, then refuse
			why := reasonSuperseded
			if !answered {
				why = reasonRunEnded
			}
			if _, _, err := s.closeTx(ctx, tx, id, ApprovalSuperseded, brokerActor, why, brokerClose(id)); err != nil {
				return envelope.Event{}, Approval{}, err
			}
			if err := tx.Commit(ctx); err != nil {
				return envelope.Event{}, Approval{}, err
			}
			return envelope.Event{}, Approval{}, ErrAlreadyDecided
		}
	}
	ev, a, err := s.closeTx(ctx, tx, id, state, by, reason, d)
	if err != nil {
		return envelope.Event{}, a, err
	}
	return ev, a, tx.Commit(ctx)
}

// closeTx moves a pending approval to state and appends its approval_decided,
// inside the caller's transaction and under the room's row lock.
func (s *Store) closeTx(ctx context.Context, tx pgx.Tx, id, state, by, reason string, d envelope.Draft) (envelope.Event, Approval, error) {
	a, err := scanApproval(tx.QueryRow(ctx, `UPDATE approvals SET state = $2, decided_by = $3, decided_at = now(),
		reason = nullif($4, '') WHERE approval_id = $1 AND state = 'pending' RETURNING `+approvalCols, id, state, by, reason))
	if errors.Is(err, pgx.ErrNoRows) {
		return envelope.Event{}, Approval{}, ErrAlreadyDecided
	}
	if err != nil {
		return envelope.Event{}, Approval{}, err
	}
	d.RoomID, d.RunID, d.Type = a.RoomID, a.RunID, envelope.ApprovalDecided
	d.Payload = envelope.Must(envelope.ApprovalDecidedPayload{ApprovalID: id, Decision: state, Reason: reason})
	ev, dup, err := s.appendTx(ctx, tx, d, fence{})
	if err == nil && dup {
		err = ErrKeyConflict
	}
	if err != nil {
		return envelope.Event{}, a, err
	}
	return ev, a, nil
}

// replayedDecision answers a replayed key: the decision it stored on this
// approval, or ErrKeyConflict for a key used by anything else.
func replayedDecision(ctx context.Context, tx pgx.Tx, prev envelope.Event, id string) (envelope.Event, Approval, error) {
	var p envelope.ApprovalDecidedPayload
	if prev.Type != envelope.ApprovalDecided || json.Unmarshal(prev.Payload, &p) != nil || p.ApprovalID != id {
		return envelope.Event{}, Approval{}, ErrKeyConflict
	}
	a, err := scanApproval(tx.QueryRow(ctx, `SELECT `+approvalCols+` FROM approvals WHERE approval_id = $1`, id))
	return prev, a, err
}

// ExpireDue closes every pending approval past its deadline as expired (§6
// Timeouts), in open rooms: a sealed room takes no event. It returns the events
// it appended; one a human decided meanwhile is skipped.
func (s *Store) ExpireDue(ctx context.Context) ([]envelope.Event, error) {
	return s.closeAll(ctx, `SELECT a.approval_id FROM approvals a JOIN rooms r USING (room_id)
		WHERE a.state = 'pending' AND NOT r.sealed AND a.expires_at < now() ORDER BY a.expires_at`,
		ApprovalExpired, reasonExpired)
}

// SupersedeAnswered closes as superseded every pending approval whose call has a
// tool_result (5.2 contract 5: the harness ran or rejected it), then every one
// whose run has ended: neither is left for approvers, nor counted as pending, nor
// holds its room AwaitingHuman.
func (s *Store) SupersedeAnswered(ctx context.Context) ([]envelope.Event, error) {
	answered, err := s.closeAll(ctx, `SELECT a.approval_id FROM approvals a JOIN rooms r USING (room_id)
		WHERE a.state = 'pending' AND NOT r.sealed AND `+answeredSQL+` ORDER BY a.requested_at`,
		ApprovalSuperseded, reasonSuperseded)
	if err != nil {
		return answered, err
	}
	ended, err := s.closeAll(ctx, `SELECT a.approval_id FROM approvals a JOIN rooms r USING (room_id)
		WHERE a.state = 'pending' AND NOT r.sealed AND `+endedSQL+` ORDER BY a.requested_at`,
		ApprovalSuperseded, reasonRunEnded)
	return append(answered, ended...), err
}

func (s *Store) closeAll(ctx context.Context, query, state, reason string) ([]envelope.Event, error) {
	rows, err := s.pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("store: approvals to close %s: %w", state, err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("store: approvals to close %s: %w", state, err)
	}
	var out []envelope.Event
	for _, id := range ids {
		ev, _, err := s.closeApproval(ctx, id, state, brokerActor, reason, brokerClose(id))
		switch {
		case errors.Is(err, ErrAlreadyDecided), errors.Is(err, ErrSealed):
			continue // decided or sealed since the read
		case err != nil:
			return out, fmt.Errorf("store: close approval %s %s: %w", id, state, err)
		}
		out = append(out, ev)
	}
	return out, nil
}

// Approval reads one approval, or ErrNoApproval.
func (s *Store) Approval(ctx context.Context, id string) (Approval, error) {
	a, err := scanApproval(s.pool.QueryRow(ctx, `SELECT `+approvalCols+` FROM approvals WHERE approval_id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Approval{}, fmt.Errorf("store: approval %s: %w", id, ErrNoApproval)
	}
	if err != nil {
		return Approval{}, fmt.Errorf("store: approval %s: %w", id, err)
	}
	return a, nil
}

// OpenApprovals lists the room's pending approvals, oldest first, for the state
// frame: a page's tail of 500 events may not reach back to a request.
func (s *Store) OpenApprovals(ctx context.Context, roomID string) ([]Approval, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+approvalCols+` FROM approvals WHERE room_id = $1 AND state = 'pending'
		ORDER BY requested_seq`, roomID)
	if err != nil {
		return nil, fmt.Errorf("store: open approvals of room %s: %w", roomID, err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Approval, error) { return scanApproval(r) })
	if err != nil {
		return nil, fmt.Errorf("store: open approvals of room %s: %w", roomID, err)
	}
	return out, nil
}

// OldestPending is the age of the oldest pending approval in an open room, and
// how many there are, on the database's clock.
func (s *Store) OldestPending(ctx context.Context) (time.Duration, int, error) {
	var secs float64
	var n int
	err := s.pool.QueryRow(ctx, `SELECT coalesce(extract(epoch FROM now() - min(a.requested_at)), 0)::float8, count(*)
		FROM approvals a JOIN rooms r USING (room_id) WHERE a.state = 'pending' AND NOT r.sealed`).Scan(&secs, &n)
	if err != nil {
		return 0, 0, fmt.Errorf("store: oldest pending approval: %w", err)
	}
	return time.Duration(secs * float64(time.Second)), n, nil
}

// Prompters are the humans who prompted a run, whom OD-16's four-eyes rule keeps
// from deciding its approvals: whoever requested it, steered it, or wrote a
// queued message its brief consumed or that was promoted to it.
func (s *Store) Prompters(ctx context.Context, roomID, runID string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT actor_id FROM events WHERE room_id = $1 AND type = 'message'
			AND actor_kind = 'human' AND payload->>'delivery' = 'steering' AND payload->'to' ? ('agent:' || $2)
		UNION SELECT actor_id FROM events WHERE room_id = $1 AND type = 'state_changed' AND actor_kind = 'human'
			AND payload->>'kind' = 'run_requested' AND payload->>'runId' = $2
		UNION SELECT author FROM queue WHERE room_id = $1 AND run_id = $2
		ORDER BY 1`, roomID, runID)
	if err != nil {
		return nil, fmt.Errorf("store: prompters of run %s: %w", runID, err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("store: prompters of run %s: %w", runID, err)
	}
	return out, nil
}
