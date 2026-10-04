-- Relational tree: the index of the recent alerts by kind that the
-- occurrence detection reads (WP-15, internal/occurrence).
--
-- alerts is a live table, written by api whenever a flight raises or
-- clears an alert, so the index is built CONCURRENTLY, which cannot run
-- in a transaction: this file runs without one, statement by statement.
-- A concurrent build that fails leaves an INVALID index of the same
-- name behind, and a database that ran 00020 before the index moved
-- here holds a valid one; the Up drops either (concurrently) and builds
-- it again, so a rerun after a failure completes.

-- +goose NO TRANSACTION
-- +goose Up
DROP INDEX CONCURRENTLY IF EXISTS alerts_kind_raised_idx;
CREATE INDEX CONCURRENTLY alerts_kind_raised_idx ON alerts (kind, raised_at);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS alerts_kind_raised_idx;
