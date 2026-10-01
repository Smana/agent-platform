-- atlas:txmode none

-- SP2 §6, phase 5. CONCURRENTLY, outside a transaction, so building them never
-- blocks appends on a populated log, like events_deliveries.
--
-- The expiry sweep and the rooms' pending counts read pending approvals only.
CREATE INDEX CONCURRENTLY approvals_pending ON approvals (room_id, expires_at) WHERE state = 'pending';

-- A bridge stream replays its run's decisions and resumes from its last
-- decision_applied, next to its deliveries and acks: store.deliverableTo and
-- lastAckSQL each name these predicates in an OR arm of their own.
CREATE INDEX CONCURRENTLY events_decisions ON events (room_id, seq) WHERE type = 'approval_decided';

CREATE INDEX CONCURRENTLY events_decision_acks ON events (room_id, run_id)
  WHERE type = 'state_changed' AND payload->>'kind' = 'decision_applied';

-- An approval whose call already has a result is superseded (store.answeredSQL).
CREATE INDEX CONCURRENTLY events_tool_results ON events (room_id, run_id, (payload->>'callId'))
  WHERE type = 'tool_result';
