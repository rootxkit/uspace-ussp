-- Relational tree: the intent projection is written after the commit
-- (WP-7 review). projected_version is the newest version of the intent
-- the KV bucket intent_active and the subject intent.v1 were given;
-- a row whose version is ahead of it was committed but not projected
-- (the bus failed after the commit), and the api's sweep republishes
-- it. A failed commit leaves nothing to project, so the projection
-- never holds an intent the database does not.

-- +goose Up
ALTER TABLE operational_intents
    ADD COLUMN projected_version integer NOT NULL DEFAULT 0 CHECK (projected_version >= 0);
-- Rows written before this migration were projected inside their
-- transaction.
UPDATE operational_intents SET projected_version = version;
CREATE INDEX operational_intents_unprojected_idx ON operational_intents (updated_at, id)
    WHERE projected_version < version;

-- +goose Down
DROP INDEX operational_intents_unprojected_idx;
ALTER TABLE operational_intents DROP COLUMN projected_version;
