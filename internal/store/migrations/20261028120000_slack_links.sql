-- Slack self-serve links (docs/superpowers/specs/2026-10-28-slack-self-serve-link-design.md): the
-- one-time codes the Slack App mints, stored by their sha256 only, and each Slack user's link to a
-- ZITADEL user. No GitHub login is stored: the factory resolves it from the sub at each use.
CREATE TABLE slack_link_codes (
  code_sha256  bytea       PRIMARY KEY CHECK (length(code_sha256) = 32),
  team_id      text        NOT NULL CHECK (team_id ~ '^T[A-Z0-9]{2,20}$'),
  user_id      text        NOT NULL CHECK (user_id ~ '^[UW][A-Z0-9]{2,20}$'),
  handle       text        NOT NULL DEFAULT '' CHECK (length(handle) <= 80),
  display_name text        NOT NULL DEFAULT '' CHECK (length(display_name) <= 80),
  expires_at   timestamptz NOT NULL,
  used_at      timestamptz
);
CREATE INDEX slack_link_codes_user ON slack_link_codes (team_id, user_id);

CREATE TABLE slack_links (
  team_id      text        NOT NULL CHECK (team_id ~ '^T[A-Z0-9]{2,20}$'),
  user_id      text        NOT NULL CHECK (user_id ~ '^[UW][A-Z0-9]{2,20}$'),
  handle       text        NOT NULL DEFAULT '' CHECK (length(handle) <= 80),
  display_name text        NOT NULL DEFAULT '' CHECK (length(display_name) <= 80),
  sub          text        NOT NULL CHECK (length(sub) BETWEEN 1 AND 128),
  linked_at    timestamptz NOT NULL,
  PRIMARY KEY (team_id, user_id)
);
CREATE INDEX slack_links_sub ON slack_links (sub);

GRANT SELECT, INSERT, UPDATE, DELETE ON slack_link_codes, slack_links TO rooms_broker;
ALTER TABLE slack_link_codes ENABLE ROW LEVEL SECURITY;
ALTER TABLE slack_links ENABLE ROW LEVEL SECURITY;
CREATE POLICY broker_slack_link_codes ON slack_link_codes FOR ALL TO rooms_broker USING (true) WITH CHECK (true);
CREATE POLICY broker_slack_links      ON slack_links      FOR ALL TO rooms_broker USING (true) WITH CHECK (true);
