-- Relational tree: validates the CHECK 00030 added NOT VALID to
-- registry_validity (WP-5, internal/registry). Rows written since 00030
-- were checked as they were written; this scans the rows that predate
-- it, each of which took the default 0.
--
-- VALIDATE CONSTRAINT holds a SHARE UPDATE EXCLUSIVE lock, which lets
-- reads and writes of registry_validity go on while it scans. The file
-- runs without a transaction, so the lock is released as the scan ends;
-- a rerun after a failure validates again at no cost.

-- +goose NO TRANSACTION
-- +goose Up
ALTER TABLE registry_validity VALIDATE CONSTRAINT registry_validity_fetched_since_check;

-- +goose Down
-- A validated constraint is not made NOT VALID again; 00030's Down drops
-- the constraint.
SELECT 1;
