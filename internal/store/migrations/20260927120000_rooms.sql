-- The room log (SP2 §4, Appendix C). Applied by Atlas as the database owner,
-- rooms_owner. The login roles rooms_broker and rooms_retention are created by
-- CNPG's managed roles, not here: rooms_owner has no CREATEROLE. Until CNPG has
-- created them the GRANTs fail and the Atlas operator retries.

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
  retention       interval    NOT NULL DEFAULT interval '90 days',
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

-- The broker appends and reads; it can never rewrite history (SC-10, T12).
GRANT SELECT, INSERT ON events TO rooms_broker;
-- INSERT on rooms: a Room CR creates its row (ruling P7).
GRANT SELECT, INSERT, UPDATE ON rooms TO rooms_broker;
-- The retention job deletes, and only what RLS below lets it see as expired.
GRANT SELECT, DELETE ON events, rooms TO rooms_retention;

ALTER TABLE events ENABLE ROW LEVEL SECURITY;
ALTER TABLE rooms ENABLE ROW LEVEL SECURITY;

CREATE POLICY broker_read_events   ON events FOR SELECT TO rooms_broker USING (true);
CREATE POLICY broker_append_events ON events FOR INSERT TO rooms_broker WITH CHECK (true);
CREATE POLICY broker_rooms         ON rooms  FOR ALL    TO rooms_broker USING (true) WITH CHECK (true);

CREATE POLICY retention_read_rooms  ON rooms  FOR SELECT TO rooms_retention USING (true);
CREATE POLICY retention_read_events ON events FOR SELECT TO rooms_retention USING (true);
CREATE POLICY retention_purge_rooms ON rooms  FOR DELETE TO rooms_retention
  USING (closed_at IS NOT NULL AND closed_at < now() - retention);
CREATE POLICY retention_purge_events ON events FOR DELETE TO rooms_retention
  USING (room_id IN (SELECT room_id FROM rooms WHERE closed_at IS NOT NULL AND closed_at < now() - retention));
