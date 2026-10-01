-- atlas:txmode none

-- SP2 §1 The brief, review 4.4 M3 and I2: start_run reads the room's latest agent
-- handoff and review verdict, its recent run requests and the runs' joins, never a
-- window of the log. store.BriefSources and store.PendingRuns imply this predicate.
-- CONCURRENTLY, outside a transaction, like events_deliveries.
CREATE INDEX CONCURRENTLY events_brief ON events (room_id, seq)
  WHERE (type = 'handoff' AND actor_kind = 'agent')
     OR (type = 'message' AND actor_kind = 'agent' AND payload->>'kind' = 'review_verdict')
     OR (type = 'state_changed' AND payload->>'kind' = 'run_requested')
     OR type = 'participant';
