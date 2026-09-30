-- Driver token and FIFO queue (SP2 §2, Appendix C). driver_seen_at is the holder's
-- connection heartbeat; driver_acted_at its last action: disconnected > 2 min or
-- idle > 15 min falls back to fallback_driver, the previous system holder.
ALTER TABLE rooms ADD COLUMN driver_acted_at timestamptz NOT NULL DEFAULT now();

-- The token moves under the same guarantees as the log (Ruling Y, T12): the broker
-- may move these columns only, and the trigger below decides which moves are legal.
GRANT UPDATE (driver, driver_epoch, fallback_driver, driver_seen_at, driver_acted_at) ON rooms TO rooms_broker;

-- The token is fenced by its epoch: it steps by one, the holder and the fallback
-- change only with it, and a sealed room's token stays where it is. The heartbeats
-- are free. Its own trigger, so the released rooms_move_forward stays untouched.
CREATE FUNCTION rooms_driver_fenced() RETURNS trigger
  LANGUAGE plpgsql SET search_path = pg_catalog, public, pg_temp AS $$
BEGIN
  IF NEW.driver_epoch NOT IN (OLD.driver_epoch, OLD.driver_epoch + 1) THEN
    RAISE EXCEPTION 'room log: driver_epoch of room % moves by one, not % to %', OLD.room_id, OLD.driver_epoch, NEW.driver_epoch
      USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.driver_epoch = OLD.driver_epoch AND (NEW.driver <> OLD.driver OR NEW.fallback_driver <> OLD.fallback_driver) THEN
    RAISE EXCEPTION 'room log: the driver of room % moves only with its epoch', OLD.room_id USING ERRCODE = 'check_violation';
  END IF;
  IF OLD.sealed AND NEW.driver_epoch <> OLD.driver_epoch THEN
    RAISE EXCEPTION 'room log: room % is sealed and its driver stays', OLD.room_id USING ERRCODE = 'check_violation';
  END IF;
  -- The fallback is the previous system holder (SP2 §2): the outgoing holder if it is
  -- a system one, otherwise the fallback stays. ChangeDriver's rule, held here too.
  -- Parenthesised: PL/pgSQL would end the IF condition at the CASE's first THEN.
  IF NEW.driver_epoch <> OLD.driver_epoch AND NEW.fallback_driver <>
      (CASE WHEN OLD.driver LIKE 'system:%' THEN OLD.driver ELSE OLD.fallback_driver END) THEN
    RAISE EXCEPTION 'room log: the fallback of room % is its previous system holder', OLD.room_id USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;

CREATE TRIGGER rooms_driver_fenced BEFORE UPDATE ON rooms
  FOR EACH ROW EXECUTE FUNCTION rooms_driver_fenced();

-- Every epoch has its driver event by commit, appended after the move (seq past the
-- row's last_seq at the time), so the token cannot move off the record, nor in a
-- sealed room, which takes no event. The event names this move, from the old holder
-- to the new one. Deferred, like rooms_seq_has_event. jsonb equality, not a cast: no
-- payload can make it fail.
CREATE FUNCTION rooms_epoch_has_event() RETURNS trigger
  LANGUAGE plpgsql SET search_path = pg_catalog, public, pg_temp AS $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM public.events WHERE room_id = NEW.room_id AND seq > NEW.last_seq
      AND type = 'driver' AND payload->'epoch' = to_jsonb(NEW.driver_epoch)
      AND payload->>'from' = OLD.driver AND payload->>'to' = NEW.driver) THEN
    RAISE EXCEPTION 'room log: driver epoch % of room % has no driver event', NEW.driver_epoch, NEW.room_id
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NULL;
END $$;

CREATE CONSTRAINT TRIGGER rooms_epoch_has_event AFTER UPDATE OF driver_epoch ON rooms
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
  WHEN (NEW.driver_epoch <> OLD.driver_epoch) EXECUTE FUNCTION rooms_epoch_has_event();

CREATE TABLE queue (
  room_id text   NOT NULL REFERENCES rooms (room_id),
  ref     bigint NOT NULL, -- the queued message's seq
  author  text   NOT NULL,
  text    text   NOT NULL,
  state   text   NOT NULL CHECK (state IN ('queued', 'removed', 'promoted', 'consumed')),
  -- the run whose brief consumed it
  run_id  text   CHECK (run_id IS NULL OR run_id ~ '^[a-z2-7]{8}$'),
  PRIMARY KEY (room_id, ref),
  -- A queued message is an event of the log: no row without one.
  FOREIGN KEY (room_id, ref) REFERENCES events (room_id, seq)
);

-- A queue row is the queued message it records: it enters with that event, by
-- its actor and with its text, so no row is planted or re-attributed. A sealed
-- room's queue neither grows nor moves.
CREATE FUNCTION queue_is_its_event() RETURNS trigger
  LANGUAGE plpgsql SET search_path = pg_catalog, public, pg_temp AS $$
BEGIN
  IF (SELECT sealed FROM public.rooms WHERE room_id = NEW.room_id) THEN
    RAISE EXCEPTION 'room log: room % is sealed and its queue stays', NEW.room_id USING ERRCODE = 'check_violation';
  END IF;
  IF TG_OP = 'INSERT' AND NOT EXISTS (SELECT 1 FROM public.events WHERE room_id = NEW.room_id AND seq = NEW.ref
      AND type = 'message' AND payload->>'delivery' = 'queued' AND actor_id = NEW.author AND payload->>'text' = NEW.text) THEN
    RAISE EXCEPTION 'room log: queue row % of room % is not its queued message', NEW.ref, NEW.room_id
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;

CREATE TRIGGER queue_is_its_event BEFORE INSERT OR UPDATE ON queue
  FOR EACH ROW EXECUTE FUNCTION queue_is_its_event();

-- The broker moves a queued message's state, never its text or author.
GRANT SELECT, INSERT ON queue TO rooms_broker;
GRANT UPDATE (state, run_id) ON queue TO rooms_broker;
-- Ruling AX: retention reads the room id of an expired room's rows, never their text.
GRANT DELETE ON queue TO rooms_retention;
GRANT SELECT (room_id) ON queue TO rooms_retention;
ALTER TABLE queue ENABLE ROW LEVEL SECURITY;
CREATE POLICY broker_queue          ON queue FOR ALL    TO rooms_broker    USING (true) WITH CHECK (true);
CREATE POLICY retention_read_queue  ON queue FOR SELECT TO rooms_retention
  USING (room_id IN (SELECT room_id FROM rooms WHERE sealed AND closed_at < now() - retention));
CREATE POLICY retention_purge_queue ON queue FOR DELETE TO rooms_retention
  USING (room_id IN (SELECT room_id FROM rooms WHERE sealed AND closed_at < now() - retention));
