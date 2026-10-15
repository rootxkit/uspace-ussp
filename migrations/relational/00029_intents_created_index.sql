-- Relational tree: the index of the console's count of the intents by
-- state (GET /v1/admin/intents/summary, internal/intent): the newest
-- intents of every operator by created_at, read at most a bounded
-- number of rows from the newest end. An operator's own newest intents
-- are on operational_intents_operator_idx (operator_id, created_at).
--
-- operational_intents is a live table, so the index is built
-- CONCURRENTLY, which cannot run in a transaction: this file runs
-- without one. A concurrent build that fails leaves an INVALID index of
-- the same name behind; the Up drops it (concurrently) and builds it
-- again, so a rerun after a failure completes.

-- +goose NO TRANSACTION
-- +goose Up
DROP INDEX CONCURRENTLY IF EXISTS operational_intents_created_idx;
CREATE INDEX CONCURRENTLY operational_intents_created_idx ON operational_intents (created_at);

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS operational_intents_created_idx;
