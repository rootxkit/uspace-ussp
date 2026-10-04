-- Relational tree: the indexes of the console's reads of alerts (WP-18,
-- internal/admin): the escalations not closed, and the alerts cleared
-- recently.
--
-- alerts is a live table, so the indexes are built CONCURRENTLY, which
-- cannot run in a transaction: this file runs without one, statement by
-- statement. A concurrent build that fails leaves an INVALID index of
-- the same name behind; the Up drops it (concurrently) and builds it
-- again, so a rerun after a failure completes.

-- +goose NO TRANSACTION
-- +goose Up
DROP INDEX CONCURRENTLY IF EXISTS alerts_escalations_idx;
CREATE INDEX CONCURRENTLY alerts_escalations_idx ON alerts (escalated_at) WHERE escalated_at IS NOT NULL AND closed_at IS NULL;
DROP INDEX CONCURRENTLY IF EXISTS alerts_cleared_idx;
CREATE INDEX CONCURRENTLY alerts_cleared_idx ON alerts (cleared_at) WHERE cleared_at IS NOT NULL;

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS alerts_cleared_idx;
DROP INDEX CONCURRENTLY IF EXISTS alerts_escalations_idx;
