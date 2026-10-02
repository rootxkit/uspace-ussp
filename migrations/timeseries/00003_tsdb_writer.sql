-- Time-series tree: what tsdb-writer needs to write the hypertables of
-- 00002 (WP-6; LESSONS B-05, B-07, B-13).
--
-- msg_id        the envelope's ULID (spec 04 §2) on every row tsdb-writer
--               writes, with a unique index on (msg_id, time column): a
--               message redelivered or republished writes nothing twice
--               (INSERT .. ON CONFLICT DO NOTHING from a staging copy).
--               A unique index on a hypertable must hold its time column.
--               The column is nullable so the migration applies to a
--               table with rows; tsdb-writer always sets it.
-- ts            the source's own clock is absent when the record carried
--               none (envelope/v1 "ts": null; LESSONS T-12); the column no
--               longer pretends otherwise.
-- accuracy_h_m, track/telemetry/v1 carries accuracies in metres; 00002's
-- accuracy_v_m  accuracy_h and accuracy_v hold F3411 category codes as
--               broadcast and stay for that.
--
-- writer_gaps       every hole in what reached the hypertables, with its
--                   cause, how much, the stream sequences and the time
--                   range around it (B-13: holes are holes). Written by
--                   tsdb-writer only, once per hole (dedupe_key), in the
--                   transaction of the rows written beside it, before the
--                   messages after the hole are acknowledged:
--                     stream_removed  the stream no longer held messages
--                                     the writer had not written (aged
--                                     out, over a limit, purged or
--                                     deleted); count in messages, one
--                                     row each
--                     malformed       a message the writer cannot read
--                     rejected        rows the database refused
--                   at is the database clock (now()).
-- writer_positions  per stream, the highest stream sequence written,
--                   committed with the rows: at start a consumer whose
--                   acknowledgement floor is beyond it lost messages to a
--                   purge or to the stream's limits while the writer was
--                   down, recorded as stream_removed.

-- +goose Up
ALTER TABLE telemetry ADD COLUMN msg_id text;
ALTER TABLE telemetry ADD COLUMN accuracy_h_m double precision CHECK (accuracy_h_m >= 0);
ALTER TABLE telemetry ADD COLUMN accuracy_v_m double precision CHECK (accuracy_v_m >= 0);
ALTER TABLE telemetry ALTER COLUMN ts DROP NOT NULL;
CREATE UNIQUE INDEX telemetry_msg_idx ON telemetry (msg_id, captured_at);

ALTER TABLE manned_tracks ADD COLUMN msg_id text;
ALTER TABLE manned_tracks ALTER COLUMN ts DROP NOT NULL;
CREATE UNIQUE INDEX manned_tracks_msg_idx ON manned_tracks (msg_id, captured_at);

ALTER TABLE econspicuity_tracks ADD COLUMN msg_id text;
ALTER TABLE econspicuity_tracks ALTER COLUMN ts DROP NOT NULL;
CREATE UNIQUE INDEX econspicuity_tracks_msg_idx ON econspicuity_tracks (msg_id, captured_at);

ALTER TABLE peer_flights ADD COLUMN msg_id text;
CREATE UNIQUE INDEX peer_flights_msg_idx ON peer_flights (msg_id, rx_ts);

ALTER TABLE traffic_products ADD COLUMN msg_id text;
CREATE UNIQUE INDEX traffic_products_msg_idx ON traffic_products (msg_id, at);

ALTER TABLE conformance_samples ADD COLUMN msg_id text;
CREATE UNIQUE INDEX conformance_samples_msg_idx ON conformance_samples (msg_id, captured_at);

CREATE TABLE writer_gaps (
    dedupe_key  text        PRIMARY KEY,
    stream      text        NOT NULL,
    subject     text        NOT NULL,
    from_seq    bigint      NOT NULL CHECK (from_seq >= 0),
    to_seq      bigint      NOT NULL CHECK (to_seq >= from_seq),
    cause       text        NOT NULL CHECK (cause IN ('stream_removed', 'malformed', 'rejected')),
    count       bigint      NOT NULL CHECK (count > 0),
    count_unit  text        NOT NULL CHECK (count_unit IN ('rows', 'messages')),
    after_at    timestamptz,
    before_at   timestamptz,
    detail      text        NOT NULL,
    at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX writer_gaps_stream_at_idx ON writer_gaps (stream, at DESC);

CREATE TABLE writer_positions (
    stream text        PRIMARY KEY,
    seq    bigint      NOT NULL CHECK (seq >= 0),
    at     timestamptz NOT NULL DEFAULT now()
);

-- +goose Down
DROP TABLE writer_positions;
DROP TABLE writer_gaps;
DROP INDEX conformance_samples_msg_idx;
ALTER TABLE conformance_samples DROP COLUMN msg_id;
DROP INDEX traffic_products_msg_idx;
ALTER TABLE traffic_products DROP COLUMN msg_id;
DROP INDEX peer_flights_msg_idx;
ALTER TABLE peer_flights DROP COLUMN msg_id;
-- A row without a source clock cannot go back under NOT NULL; Down
-- refuses rather than invent one.
DROP INDEX econspicuity_tracks_msg_idx;
ALTER TABLE econspicuity_tracks ALTER COLUMN ts SET NOT NULL;
ALTER TABLE econspicuity_tracks DROP COLUMN msg_id;
DROP INDEX manned_tracks_msg_idx;
ALTER TABLE manned_tracks ALTER COLUMN ts SET NOT NULL;
ALTER TABLE manned_tracks DROP COLUMN msg_id;
DROP INDEX telemetry_msg_idx;
ALTER TABLE telemetry ALTER COLUMN ts SET NOT NULL;
ALTER TABLE telemetry DROP COLUMN accuracy_v_m;
ALTER TABLE telemetry DROP COLUMN accuracy_h_m;
ALTER TABLE telemetry DROP COLUMN msg_id;
