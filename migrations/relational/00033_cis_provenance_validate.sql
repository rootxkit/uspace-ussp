-- Relational tree: validates the CHECK 00032 added NOT VALID to
-- cis_datasets (WP-4, internal/cis provenance.go). Rows written since
-- 00032 were checked as they were written; this scans the rows that
-- predate it, each of which 00032 backfilled.
--
-- VALIDATE CONSTRAINT holds a SHARE UPDATE EXCLUSIVE lock, which lets
-- reads and writes of cis_datasets go on while it scans. The file runs
-- without a transaction, so the lock is released as the scan ends; a
-- rerun after a failure validates again at no cost.

-- +goose NO TRANSACTION
-- +goose Up
ALTER TABLE cis_datasets VALIDATE CONSTRAINT cis_datasets_provenance_check;

-- +goose Down
-- A validated constraint is not made NOT VALID again; 00032's Down drops
-- the constraint.
SELECT 1;
