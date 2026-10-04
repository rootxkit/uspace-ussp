-- Relational tree: what a service record needs that nothing kept yet
-- (WP-15; spec 02 F7, LESSONS B-13).
--
-- ingest_gaps  every gap record telemetry-ingest publishes on src.v1
--              (telemetry.Gap: samples dropped from its work queue, with
--              the cause, the client, when the dropped samples were
--              captured and how many), recorded by api as it hears them,
--              once per message (msg_id). A record names one of them as
--              the cause of a hole in a flight's telemetry that it
--              overlaps; a hole nothing explains says "no recorded
--              cause". A gap published while api was not listening is
--              not here, and a record says so the same way (B-13: a hole
--              is a hole). gap_started and gap_ended are NULL for samples
--              the queue removed before anyone read them.
-- record_bundles gains a check that content_hash is a SHA-256 in hex.

-- +goose Up
CREATE TABLE ingest_gaps (
    id              bigserial   PRIMARY KEY,
    msg_id          text        NOT NULL UNIQUE,
    source_instance text        NOT NULL,
    cause           text        NOT NULL,
    gap_started     timestamptz,
    gap_ended       timestamptz,
    dropped         integer     NOT NULL CHECK (dropped >= 0),
    from_seq        bigint      CHECK (from_seq >= 0),
    to_seq          bigint      CHECK (to_seq >= 0),
    recorded_at     timestamptz NOT NULL DEFAULT now(),
    CHECK ((gap_started IS NULL) = (gap_ended IS NULL)),
    CHECK (gap_ended IS NULL OR gap_ended >= gap_started)
);
CREATE INDEX ingest_gaps_instance_idx ON ingest_gaps (source_instance, gap_started);
CREATE INDEX ingest_gaps_recorded_idx ON ingest_gaps (recorded_at);

ALTER TABLE record_bundles
    ADD CONSTRAINT record_bundles_hash_check CHECK (content_hash ~ '^[0-9a-f]{64}$');

GRANT SELECT, INSERT, DELETE ON ingest_gaps TO ussp_app;
GRANT USAGE, SELECT ON SEQUENCE ingest_gaps_id_seq TO ussp_app;

-- +goose Down
ALTER TABLE record_bundles DROP CONSTRAINT record_bundles_hash_check;
DROP TABLE ingest_gaps;
