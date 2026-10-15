-- Written on every claim and renewal, read nowhere since ruling SBB made freshness
-- irrelevant (review 2026-10-15, B11). Dropping the column also drops it from the
-- rooms_broker column-level UPDATE grant; nothing is widened.
ALTER TABLE rooms DROP COLUMN bridge_seen_at;
