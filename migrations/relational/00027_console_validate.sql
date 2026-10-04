-- Relational tree: validates the three CHECKs 00025 added NOT VALID to
-- alerts (WP-18, internal/admin). Rows written since 00025 were checked
-- as they were written; this scans the rows that predate it.
--
-- VALIDATE CONSTRAINT holds a SHARE UPDATE EXCLUSIVE lock, which lets
-- reads and writes of alerts go on while it scans. The file runs without
-- a transaction, one statement at a time, so each lock is released as
-- its scan ends; a rerun after a failure validates what is left (a valid
-- constraint validates again at no cost).

-- +goose NO TRANSACTION
-- +goose Up
ALTER TABLE alerts VALIDATE CONSTRAINT alerts_messages_recorded_check;
ALTER TABLE alerts VALIDATE CONSTRAINT alerts_escalated_by_check;
ALTER TABLE alerts VALIDATE CONSTRAINT alerts_closed_check;

-- +goose Down
-- A validated constraint is not made NOT VALID again; 00025's Down drops
-- the constraints.
SELECT 1;
