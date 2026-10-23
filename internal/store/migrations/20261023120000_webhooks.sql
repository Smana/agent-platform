-- Webhook delivery state (factory interfaces F6, R17, R18): where each subscriber stands in each
-- room, and whether it is disabled. Written by the broker's leader only; no event content.
CREATE TABLE webhook_cursors (
  subscriber text   NOT NULL CHECK (subscriber ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'),
  room_id    text   NOT NULL REFERENCES rooms (room_id),
  seq        bigint NOT NULL CHECK (seq >= 0),
  PRIMARY KEY (subscriber, room_id)
);

CREATE TABLE webhook_subscribers (
  subscriber  text        PRIMARY KEY CHECK (subscriber ~ '^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$'),
  generation  integer     NOT NULL DEFAULT 0,
  failures    integer     NOT NULL DEFAULT 0 CHECK (failures >= 0),
  disabled_at timestamptz
);

GRANT SELECT, INSERT, UPDATE ON webhook_cursors, webhook_subscribers TO rooms_broker;
-- Retention reads the room id of an expired room's cursors and deletes them before the room.
GRANT DELETE ON webhook_cursors TO rooms_retention;
GRANT SELECT (room_id) ON webhook_cursors TO rooms_retention;
ALTER TABLE webhook_cursors ENABLE ROW LEVEL SECURITY;
ALTER TABLE webhook_subscribers ENABLE ROW LEVEL SECURITY;
CREATE POLICY broker_webhook_cursors          ON webhook_cursors     FOR ALL    TO rooms_broker    USING (true) WITH CHECK (true);
CREATE POLICY broker_webhook_subscribers      ON webhook_subscribers FOR ALL    TO rooms_broker    USING (true) WITH CHECK (true);
CREATE POLICY retention_read_webhook_cursors  ON webhook_cursors     FOR SELECT TO rooms_retention
  USING (room_id IN (SELECT room_id FROM rooms WHERE sealed AND closed_at < now() - retention));
CREATE POLICY retention_purge_webhook_cursors ON webhook_cursors     FOR DELETE TO rooms_retention
  USING (room_id IN (SELECT room_id FROM rooms WHERE sealed AND closed_at < now() - retention));
