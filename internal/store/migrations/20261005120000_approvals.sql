-- Approvals (SP2 §6, Appendix C): the projection behind first-decision-wins. An
-- approval_requested event opens a row and an approval_decided event closes it, once.
-- A request is idempotent per (room, run, event_id), never per call_id: a model
-- provider may reuse a tool call id (5.2 review I2). prompters: the humans who
-- prompted the run (its principal, whoever steered it, the authors of the queued
-- messages its brief consumed), whom OD-16's four-eyes rule keeps from deciding.
CREATE TABLE approvals (
  approval_id   text        PRIMARY KEY,
  room_id       text        NOT NULL REFERENCES rooms (room_id),
  run_id        text        NOT NULL CHECK (run_id ~ '^[a-z2-7]{8}$'),
  event_id      text        NOT NULL,
  call_id       text        NOT NULL,
  class         text        NOT NULL,
  action        jsonb       NOT NULL,
  prompters     text[]      NOT NULL DEFAULT '{}',
  state         text        NOT NULL CHECK (state IN ('pending', 'approved', 'denied', 'expired', 'superseded')),
  requested_seq bigint      NOT NULL,
  requested_at  timestamptz NOT NULL DEFAULT now(),
  expires_at    timestamptz NOT NULL,
  decided_by    text,
  decided_at    timestamptz,
  reason        text,
  UNIQUE (room_id, run_id, event_id),
  -- An approval is an event of the log: no row without its request.
  FOREIGN KEY (room_id, requested_seq) REFERENCES events (room_id, seq)
);

-- A row is the approval_requested event it records: it enters pending with that
-- event, for its run, call, class and action, so no approval is planted or
-- reworded. It moves once, out of pending, at now(), naming who closed it. A
-- sealed room's approvals stay as they are.
CREATE FUNCTION approvals_are_their_events() RETURNS trigger
  LANGUAGE plpgsql SET search_path = pg_catalog, public, pg_temp AS $$
BEGIN
  IF (SELECT sealed FROM public.rooms WHERE room_id = NEW.room_id) THEN
    RAISE EXCEPTION 'room log: room % is sealed and its approvals stay', NEW.room_id USING ERRCODE = 'check_violation';
  END IF;
  IF TG_OP = 'INSERT' AND (NEW.state <> 'pending' OR NEW.decided_by IS NOT NULL OR NEW.decided_at IS NOT NULL
      OR NEW.reason IS NOT NULL OR NEW.requested_at <> now()) THEN
    RAISE EXCEPTION 'room log: approval % enters pending, undecided, at now()', NEW.approval_id USING ERRCODE = 'check_violation';
  END IF;
  IF TG_OP = 'INSERT' AND NOT EXISTS (SELECT 1 FROM public.events WHERE room_id = NEW.room_id AND seq = NEW.requested_seq
      AND type = 'approval_requested' AND run_id = NEW.run_id AND payload->>'approvalId' = NEW.approval_id
      AND payload->>'callId' = NEW.call_id AND payload->>'class' = NEW.class AND payload->'action' = NEW.action) THEN
    RAISE EXCEPTION 'room log: approval % is not its approval_requested event', NEW.approval_id
      USING ERRCODE = 'check_violation';
  END IF;
  IF TG_OP = 'UPDATE' AND (OLD.state <> 'pending' OR NEW.state = 'pending') THEN
    RAISE EXCEPTION 'room log: approval % moves once, out of pending (% to %)', OLD.approval_id, OLD.state, NEW.state
      USING ERRCODE = 'check_violation';
  END IF;
  IF TG_OP = 'UPDATE' AND (NEW.decided_by IS NULL OR NEW.decided_at IS DISTINCT FROM now()) THEN
    RAISE EXCEPTION 'room log: approval % is closed at now(), by someone', OLD.approval_id USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;

CREATE TRIGGER approvals_are_their_events BEFORE INSERT OR UPDATE ON approvals
  FOR EACH ROW EXECUTE FUNCTION approvals_are_their_events();

-- Every close has its approval_decided event by commit, appended after the request,
-- naming this approval and this outcome, so no approval closes off the record.
-- Deferred, like rooms_epoch_has_event.
CREATE FUNCTION approvals_decision_has_event() RETURNS trigger
  LANGUAGE plpgsql SET search_path = pg_catalog, public, pg_temp AS $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM public.events WHERE room_id = NEW.room_id AND seq > NEW.requested_seq
      AND type = 'approval_decided' AND run_id = NEW.run_id AND payload->>'approvalId' = NEW.approval_id
      AND payload->>'decision' = NEW.state) THEN
    RAISE EXCEPTION 'room log: approval % closed % has no approval_decided event', NEW.approval_id, NEW.state
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NULL;
END $$;

CREATE CONSTRAINT TRIGGER approvals_decision_has_event AFTER UPDATE OF state ON approvals
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
  WHEN (NEW.state <> OLD.state) EXECUTE FUNCTION approvals_decision_has_event();

-- The broker closes an approval, never rewrites what it asked.
GRANT SELECT, INSERT ON approvals TO rooms_broker;
GRANT UPDATE (state, decided_by, decided_at, reason) ON approvals TO rooms_broker;
-- Ruling AX: retention reads the room id of an expired room's rows, never an action.
GRANT DELETE ON approvals TO rooms_retention;
GRANT SELECT (room_id) ON approvals TO rooms_retention;
ALTER TABLE approvals ENABLE ROW LEVEL SECURITY;
CREATE POLICY broker_approvals          ON approvals FOR ALL    TO rooms_broker    USING (true) WITH CHECK (true);
CREATE POLICY retention_read_approvals  ON approvals FOR SELECT TO rooms_retention
  USING (room_id IN (SELECT room_id FROM rooms WHERE sealed AND closed_at < now() - retention));
CREATE POLICY retention_purge_approvals ON approvals FOR DELETE TO rooms_retention
  USING (room_id IN (SELECT room_id FROM rooms WHERE sealed AND closed_at < now() - retention));
