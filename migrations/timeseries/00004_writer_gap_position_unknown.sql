-- Time-series tree: the position_unknown cause of writer_gaps (audit
-- S3; LESSONS B-13). tsdb-writer records it when its durable consumer
-- acknowledged messages (an ack floor above zero) and the database
-- holds no writer_positions row for the stream: a database restored
-- from an older backup or re-created. Those rows are in no database,
-- and the gap says so instead of starting clean.

-- +goose Up
ALTER TABLE writer_gaps DROP CONSTRAINT writer_gaps_cause_check;
ALTER TABLE writer_gaps ADD CONSTRAINT writer_gaps_cause_check
    CHECK (cause IN ('stream_removed', 'malformed', 'rejected', 'position_unknown'));

-- +goose Down
-- A position_unknown row cannot go back under the narrower check; Down
-- refuses (the ALTER fails) rather than delete a record.
ALTER TABLE writer_gaps DROP CONSTRAINT writer_gaps_cause_check;
ALTER TABLE writer_gaps ADD CONSTRAINT writer_gaps_cause_check
    CHECK (cause IN ('stream_removed', 'malformed', 'rejected'));
