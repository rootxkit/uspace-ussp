-- Time-series tree (TimescaleDB), database ussp_timeseries.
-- goose version table: goose_db_version_timeseries (docs/PLAN.md D5).
-- Never run against ussp_relational; the two trees are never merged.
--
-- ussp_tsdb owns the database and runs this tree (`ussp-tsdb-writer
-- migrate`); tsdb-writer is the only writer of the hypertables. ussp_api
-- reads them for records through the default privileges of
-- deploy/compose/initdb/10-ussp.sql. Both extensions are created there
-- as the superuser (neither is trusted); this migration checks and names
-- the script instead of creating them.

-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'timescaledb') THEN
        RAISE EXCEPTION 'extension timescaledb is not installed in this database: deploy/compose/initdb/10-ussp.sql creates it as the superuser';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'postgis') THEN
        RAISE EXCEPTION 'extension postgis is not installed in this database: deploy/compose/initdb/10-ussp.sql creates it as the superuser';
    END IF;
END $$;
-- +goose StatementEnd

-- +goose Down
-- Nothing to undo: the extensions belong to the bootstrap script.
SELECT 1;
