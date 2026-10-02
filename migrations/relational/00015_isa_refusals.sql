-- Relational tree: an ISA write the DSS keeps refusing is given up
-- (WP-9 review). refusals counts the DSS answers in a row that refused
-- a put or a delete of the ISA (a 4xx, an answer that cannot be used, a
-- version conflict); a DSS that does not answer is not a refusal. At
-- the worker's bound the item is done and refused_at is set, which
-- /readyz reports until the ISA is written or deleted.

-- +goose Up
ALTER TABLE dss_isas
    ADD COLUMN refusals   integer NOT NULL DEFAULT 0 CHECK (refusals >= 0),
    ADD COLUMN refused_at timestamptz;
CREATE INDEX dss_isas_refused_idx ON dss_isas (refused_at) WHERE refused_at IS NOT NULL AND deleted_at IS NULL;

-- +goose Down
DROP INDEX dss_isas_refused_idx;
ALTER TABLE dss_isas DROP COLUMN refused_at, DROP COLUMN refusals;
