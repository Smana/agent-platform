-- atlas:txmode none

-- SP2 §2, review 4.3 I3: a bridge's stream replays its run's deliveries and
-- resumes from its last acknowledgement on every connect. Both read through
-- these, never the room's whole log; store.deliverableTo and lastAckSQL imply
-- their predicates. CONCURRENTLY, outside a transaction, so building them never
-- blocks appends on a populated log, like events_agent_verdicts.
CREATE INDEX CONCURRENTLY events_deliveries ON events (room_id, seq)
  WHERE (type = 'message' AND payload->>'delivery' = 'steering')
     OR (type = 'state_changed' AND payload->>'kind' = 'interrupt');

CREATE INDEX CONCURRENTLY events_acks ON events (room_id, run_id)
  WHERE type = 'state_changed' AND payload->>'kind' IN ('delivered', 'interrupted', 'undeliverable');
