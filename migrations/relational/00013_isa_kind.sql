-- Relational tree: the kind of a flight's F3411 Identification Service
-- Area (WP-9). An intent's ISA spans the intent's volumes and window; a
-- session ISA (a flight without an intent) reaches session_isa_horizon_s
-- ahead and is renewed while the flight goes on, so the worker must
-- tell them apart. Rows before this migration (none were written) are
-- intent ISAs.

-- +goose Up
ALTER TABLE dss_isas
    ADD COLUMN kind text NOT NULL DEFAULT 'intent' CHECK (kind IN ('intent', 'session'));
CREATE INDEX dss_isas_open_idx ON dss_isas (time_end) WHERE deleted_at IS NULL;

-- +goose Down
DROP INDEX dss_isas_open_idx;
ALTER TABLE dss_isas DROP COLUMN kind;
