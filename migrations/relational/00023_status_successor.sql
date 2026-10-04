-- Relational tree: at most one standing successor per operating-status
-- notice (WP-15 review; internal/status).
--
-- follows is the notice a cease or a restart was asked after (the
-- newest notice of its certificate that had not failed when it was
-- stored); a start follows none. The unique index lets one notice that
-- has not failed follow each notice, so two requests for the same change
-- that read the notices before either stored its own (two consoles, one
-- click each) store one notice: the second insert is refused and the
-- request reads again and answers the first. Rows stored before this
-- migration follow none and so never collide.
--
-- operating_status_notices is written by api from 00021 on, so the
-- index is built CONCURRENTLY, outside a transaction (a plain build
-- would stop every write to the table for its duration); the Up drops
-- an INVALID index a failed build left before building it again.
-- Adding a nullable column without a default rewrites nothing.

-- +goose NO TRANSACTION
-- +goose Up
ALTER TABLE operating_status_notices ADD COLUMN IF NOT EXISTS follows uuid;
DROP INDEX CONCURRENTLY IF EXISTS operating_status_notices_one_successor_idx;
CREATE UNIQUE INDEX CONCURRENTLY operating_status_notices_one_successor_idx
    ON operating_status_notices (follows) WHERE state <> 'failed';

-- +goose Down
DROP INDEX CONCURRENTLY IF EXISTS operating_status_notices_one_successor_idx;
ALTER TABLE operating_status_notices DROP COLUMN IF EXISTS follows;
