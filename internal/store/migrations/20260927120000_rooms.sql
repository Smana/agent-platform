-- The room log (SP2 §4, Appendix C). Applied by Atlas as the database owner,
-- rooms_owner. The login roles rooms_broker and rooms_retention are created by
-- CNPG's managed roles, not here: rooms_owner has no CREATEROLE. Until CNPG has
-- created them the GRANTs fail and the Atlas operator retries.
--
-- Nothing can rewrite history (T12, SC-10): the grants, row-level security and
-- triggers below hold the invariants even against the broker's own credential.
-- The functions run as their caller, are owned by rooms_owner (so no login role can
-- replace or drop them), and name every table by schema with a pinned search_path,
-- so a temporary table cannot shadow the one they check.

-- The per-room sequencer. `last_seq` is incremented under the row lock, so
-- writers serialise per room, and a rolled-back append also undoes the counter.
CREATE TABLE rooms (
  room_id         text        PRIMARY KEY CHECK (room_id ~ '^[a-z2-7]{8}$'),
  last_seq        bigint      NOT NULL DEFAULT 0,
  bytes           bigint      NOT NULL DEFAULT 0,
  driver          text        NOT NULL,
  driver_epoch    bigint      NOT NULL DEFAULT 0,
  fallback_driver text        NOT NULL,
  driver_seen_at  timestamptz NOT NULL DEFAULT now(),
  last_event_at   timestamptz NOT NULL DEFAULT now(),
  -- The run whose bridge holds the room (ruling P17), shared by every broker replica.
  bridge_run      text        CHECK (bridge_run IS NULL OR bridge_run ~ '^[a-z2-7]{8}$'),
  bridge_seen_at  timestamptz,
  sealed          boolean     NOT NULL DEFAULT false,
  closed_at       timestamptz,
  -- A day at least, like the Room CRD's <n>d: a shorter one would purge a log
  -- before anyone read it (Ruling AE).
  retention       interval    NOT NULL DEFAULT interval '90 days' CHECK (retention >= interval '1 day'),
  created_at      timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE events (
  room_id       text        NOT NULL REFERENCES rooms (room_id),
  seq           bigint      NOT NULL CHECK (seq > 0),
  id            text        NOT NULL,
  run_id        text        CHECK (run_id IS NULL OR run_id ~ '^[a-z2-7]{8}$'),
  actor_kind    text        NOT NULL CHECK (actor_kind IN ('agent', 'human', 'system')),
  actor_id      text        NOT NULL,
  actor_role    text,
  type          text        NOT NULL,
  caused_by     bigint,
  origin        text        NOT NULL CHECK (origin IN ('harness', 'broker', 'client')),
  origin_client text        NOT NULL,
  origin_seq    bigint      NOT NULL,
  ts            timestamptz NOT NULL,
  redactions    text[]      NOT NULL DEFAULT '{}',
  payload       jsonb       NOT NULL,
  PRIMARY KEY (room_id, seq),
  UNIQUE (room_id, origin_client, origin_seq)
);

-- A room only moves forward: its id, retention and creation never change, last_seq
-- steps by one, bytes never shrink, and it is sealed and closed once, at now().
CREATE FUNCTION rooms_move_forward() RETURNS trigger
  LANGUAGE plpgsql SET search_path = pg_catalog, public, pg_temp AS $$
BEGIN
  IF NEW.room_id <> OLD.room_id OR NEW.retention <> OLD.retention OR NEW.created_at <> OLD.created_at THEN
    RAISE EXCEPTION 'room log: room_id, retention and created_at never change' USING ERRCODE = 'check_violation';
  END IF;
  IF OLD.sealed AND NOT NEW.sealed THEN
    RAISE EXCEPTION 'room log: room % stays sealed', OLD.room_id USING ERRCODE = 'check_violation';
  END IF;
  IF OLD.sealed AND (NEW.last_seq <> OLD.last_seq OR NEW.bytes <> OLD.bytes) THEN
    RAISE EXCEPTION 'room log: room % is sealed and takes no event', OLD.room_id USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.last_seq NOT IN (OLD.last_seq, OLD.last_seq + 1) THEN
    RAISE EXCEPTION 'room log: last_seq of room % moves by one, not % to %', OLD.room_id, OLD.last_seq, NEW.last_seq
      USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.bytes < OLD.bytes THEN
    RAISE EXCEPTION 'room log: bytes of room % never shrink', OLD.room_id USING ERRCODE = 'check_violation';
  END IF;
  IF OLD.closed_at IS NOT NULL AND NEW.closed_at IS DISTINCT FROM OLD.closed_at THEN
    RAISE EXCEPTION 'room log: closed_at of room % is set once', OLD.room_id USING ERRCODE = 'check_violation';
  END IF;
  IF OLD.closed_at IS NULL AND NEW.closed_at IS NOT NULL AND (NEW.closed_at <> now() OR NOT NEW.sealed) THEN
    RAISE EXCEPTION 'room log: closed_at of room % is now(), set when sealing', OLD.room_id USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.sealed AND NEW.closed_at IS NULL THEN
    RAISE EXCEPTION 'room log: sealing room % sets closed_at', OLD.room_id USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;

CREATE TRIGGER rooms_move_forward BEFORE UPDATE ON rooms
  FOR EACH ROW EXECUTE FUNCTION rooms_move_forward();

-- Gapless: every seq a room takes has its event by commit, so last_seq cannot be
-- advanced alone. Deferred, because the append increments before it inserts.
CREATE FUNCTION rooms_seq_has_event() RETURNS trigger
  LANGUAGE plpgsql SET search_path = pg_catalog, public, pg_temp AS $$
BEGIN
  IF NOT EXISTS (SELECT 1 FROM public.events WHERE room_id = NEW.room_id AND seq = NEW.last_seq) THEN
    RAISE EXCEPTION 'room log: seq % of room % has no event', NEW.last_seq, NEW.room_id USING ERRCODE = 'check_violation';
  END IF;
  RETURN NULL;
END $$;

CREATE CONSTRAINT TRIGGER rooms_seq_has_event AFTER UPDATE OF last_seq ON rooms
  DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
  WHEN (NEW.last_seq <> OLD.last_seq) EXECUTE FUNCTION rooms_seq_has_event();

-- An event takes exactly the seq its room just reached, and never enters a sealed room.
CREATE FUNCTION events_take_next_seq() RETURNS trigger
  LANGUAGE plpgsql SET search_path = pg_catalog, public, pg_temp AS $$
DECLARE
  next_seq bigint;
  is_sealed boolean;
BEGIN
  SELECT last_seq, sealed INTO next_seq, is_sealed FROM public.rooms WHERE room_id = NEW.room_id;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'room log: no room %', NEW.room_id USING ERRCODE = 'foreign_key_violation';
  END IF;
  IF is_sealed THEN
    RAISE EXCEPTION 'room log: room % is sealed', NEW.room_id USING ERRCODE = 'check_violation';
  END IF;
  IF NEW.seq <> next_seq THEN
    RAISE EXCEPTION 'room log: seq % is not the next of room % (last_seq %)', NEW.seq, NEW.room_id, next_seq
      USING ERRCODE = 'check_violation';
  END IF;
  RETURN NEW;
END $$;

CREATE TRIGGER events_take_next_seq BEFORE INSERT ON events
  FOR EACH ROW EXECUTE FUNCTION events_take_next_seq();

-- Events are never rewritten or truncated, whoever asks. Only retention deletes them.
CREATE FUNCTION events_are_immutable() RETURNS trigger
  LANGUAGE plpgsql SET search_path = pg_catalog, public, pg_temp AS $$
BEGIN
  RAISE EXCEPTION 'room log: events are append-only (%)', TG_OP USING ERRCODE = 'check_violation';
END $$;

CREATE TRIGGER events_no_update BEFORE UPDATE ON events
  FOR EACH ROW EXECUTE FUNCTION events_are_immutable();
CREATE TRIGGER events_no_truncate BEFORE TRUNCATE ON events
  FOR EACH STATEMENT EXECUTE FUNCTION events_are_immutable();

-- The broker appends and reads; it can never rewrite history (SC-10, T12).
GRANT SELECT, INSERT ON events TO rooms_broker;
-- A Room CR creates its row (ruling P7); every other column takes its default.
GRANT SELECT ON rooms TO rooms_broker;
GRANT INSERT (room_id, driver, fallback_driver, retention) ON rooms TO rooms_broker;
-- Only the columns the store moves: the sequencer, the seal and the bridge lease.
GRANT UPDATE (last_seq, bytes, last_event_at, sealed, closed_at, bridge_run, bridge_seen_at) ON rooms TO rooms_broker;
-- The retention job deletes, and only what RLS below lets it see as expired.
GRANT SELECT, DELETE ON events, rooms TO rooms_retention;

ALTER TABLE events ENABLE ROW LEVEL SECURITY;
ALTER TABLE rooms ENABLE ROW LEVEL SECURITY;

CREATE POLICY broker_read_events   ON events FOR SELECT TO rooms_broker USING (true);
CREATE POLICY broker_append_events ON events FOR INSERT TO rooms_broker WITH CHECK (true);
CREATE POLICY broker_read_rooms    ON rooms  FOR SELECT TO rooms_broker USING (true);
CREATE POLICY broker_create_rooms  ON rooms  FOR INSERT TO rooms_broker
  WITH CHECK (last_seq = 0 AND bytes = 0 AND NOT sealed AND closed_at IS NULL AND bridge_run IS NULL);
-- Permissive on purpose: a policy cannot compare a row with its previous version.
-- The column grants and rooms_move_forward decide which moves are legal.
CREATE POLICY broker_move_rooms    ON rooms  FOR UPDATE TO rooms_broker USING (true) WITH CHECK (true);

CREATE POLICY retention_read_rooms  ON rooms  FOR SELECT TO rooms_retention USING (true);
CREATE POLICY retention_read_events ON events FOR SELECT TO rooms_retention USING (true);
CREATE POLICY retention_purge_rooms ON rooms  FOR DELETE TO rooms_retention
  USING (sealed AND closed_at < now() - retention);
CREATE POLICY retention_purge_events ON events FOR DELETE TO rooms_retention
  USING (room_id IN (SELECT room_id FROM rooms WHERE sealed AND closed_at < now() - retention));
