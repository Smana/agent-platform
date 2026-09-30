-- SP2 design §3: the leader finds agents' review verdicts that have not reached GitHub
-- yet. Only verdict rows are indexed. The "already recorded" check rides on the
-- existing UNIQUE (room_id, origin_client, origin_seq).
CREATE INDEX events_agent_verdicts ON events (ts)
  WHERE type = 'message' AND actor_kind = 'agent' AND payload->>'kind' = 'review_verdict';
