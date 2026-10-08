-- atlas:txmode none

-- roomctrl reads each room's latest task facts every reconcile (store.LastTaskState). Without
-- this the lookup walks the room's whole log backward; the predicate there must match verbatim.
CREATE INDEX CONCURRENTLY events_task_facts ON events (room_id, seq)
  WHERE type = 'state_changed' AND payload->>'kind' = 'task';
