-- Time-series tree: the hypertables of docs/PLAN.md §5.2.
--
-- Chunks are 1 day, compressed after 7 days (segmented by the track or
-- flight key, ordered by time) and dropped after 90 days, except:
--
--   peer_flights  peer data is held at most 24 h (uspace-core f3411
--                 NetDpMaxDataRetentionPeriodSeconds, a standard limit,
--                 never policy; a test compares the job with it). It uses 1-hour chunks
--                 and a retention job every 15 minutes, because a
--                 retention policy drops whole chunks only: with 1-day
--                 chunks a row could outlive the limit by a day. A row is
--                 gone at most 24 h + 1 h chunk + 15 min after rx_ts and
--                 is never dropped before 24 h. It is never compressed.
--   telemetry     90 days is the default of the policy row
--                 (telemetry_retention_days, floor 30 days, Art.
--                 15(1)(g)); tsdb-writer replaces this retention policy
--                 when the policy changes.
--
-- Positions are PostGIS geometry(Point, 4326) (the extension is created
-- by deploy/compose/initdb/10-ussp.sql). Units and datums are in the
-- column names: alt_wgs84_m, alt_amsl_m, alt_pressure_m; height_m is the
-- broadcast's own field and travels with height_ref, never a judged AGL
-- (D-02). accuracy_h and accuracy_v are the F3411 accuracy categories as
-- broadcast (codes, not metres).

-- +goose Up
CREATE TABLE telemetry (
    flight_id            uuid        NOT NULL,
    captured_at          timestamptz NOT NULL,
    ts                   timestamptz NOT NULL,
    rx_ts                timestamptz NOT NULL,
    backlog              boolean     NOT NULL DEFAULT false,
    time_source          text        NOT NULL,
    geom                 geometry(Point, 4326) NOT NULL,
    alt_wgs84_m          double precision,
    alt_amsl_m           double precision,
    undulation_m         double precision,
    alt_pressure_m       double precision,
    height_m             double precision,
    height_ref           text,
    speed_ms             double precision,
    track_deg            double precision CHECK (track_deg >= 0 AND track_deg < 360),
    vspeed_ms            double precision,
    status               text,
    emergency            boolean     NOT NULL DEFAULT false,
    operator_position    geometry(Point, 4326),
    accuracy_h           text,
    accuracy_v           text,
    timestamp_accuracy_s double precision,
    source_client_id     text,
    cell5                text        NOT NULL,
    CHECK ((height_m IS NULL) = (height_ref IS NULL))
);
SELECT create_hypertable('telemetry', by_range('captured_at', INTERVAL '1 day'));
CREATE INDEX telemetry_flight_idx ON telemetry (flight_id, captured_at);
ALTER TABLE telemetry SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'flight_id',
    timescaledb.compress_orderby = 'captured_at'
);
SELECT add_compression_policy('telemetry', INTERVAL '7 days');
SELECT add_retention_policy('telemetry', INTERVAL '90 days');

-- Peer flights seen through the DSS (F3411 RIDAircraftState verbatim).
CREATE TABLE peer_flights (
    peer_uss      text        NOT NULL,
    rid_flight_id text        NOT NULL,
    rx_ts         timestamptz NOT NULL,
    state         jsonb       NOT NULL,
    details       jsonb,
    isa_id        text,
    cell5         text
);
SELECT create_hypertable('peer_flights', by_range('rx_ts', INTERVAL '1 hour'));
CREATE INDEX peer_flights_flight_idx ON peer_flights (peer_uss, rid_flight_id, rx_ts);
SELECT add_retention_policy('peer_flights', INTERVAL '24 hours', schedule_interval => INTERVAL '15 minutes');

-- Manned traffic from the ANSP stream (F4).
CREATE TABLE manned_tracks (
    icao24         text        NOT NULL,
    callsign       text,
    captured_at    timestamptz NOT NULL,
    ts             timestamptz NOT NULL,
    rx_ts          timestamptz NOT NULL,
    geom           geometry(Point, 4326) NOT NULL,
    alt_pressure_m double precision,
    alt_wgs84_m    double precision,
    gs_ms          double precision,
    track_deg      double precision CHECK (track_deg >= 0 AND track_deg < 360),
    vrate_ms       double precision,
    emergency      text,
    source_class   text        NOT NULL,
    quality        text,
    adapter_id     text        NOT NULL,
    trust          text        NOT NULL,
    cell5          text        NOT NULL
);
SELECT create_hypertable('manned_tracks', by_range('captured_at', INTERVAL '1 day'));
CREATE INDEX manned_tracks_icao24_idx ON manned_tracks (icao24, captured_at);
ALTER TABLE manned_tracks SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'icao24',
    timescaledb.compress_orderby = 'captured_at'
);
SELECT add_compression_policy('manned_tracks', INTERVAL '7 days');
SELECT add_retention_policy('manned_tracks', INTERVAL '90 days');

-- e-conspicuity (ADS-B and the like) from a receiver: trust is always
-- broadcast.
CREATE TABLE econspicuity_tracks (
    icao24         text        NOT NULL,
    callsign       text,
    captured_at    timestamptz NOT NULL,
    ts             timestamptz NOT NULL,
    rx_ts          timestamptz NOT NULL,
    geom           geometry(Point, 4326) NOT NULL,
    alt_pressure_m double precision,
    alt_wgs84_m    double precision,
    gs_ms          double precision,
    track_deg      double precision CHECK (track_deg >= 0 AND track_deg < 360),
    vrate_ms       double precision,
    emergency      text,
    source_class   text        NOT NULL,
    quality        text,
    adapter_id     text        NOT NULL,
    trust          text        NOT NULL CHECK (trust = 'broadcast'),
    receiver_id    text        NOT NULL,
    cell5          text        NOT NULL
);
SELECT create_hypertable('econspicuity_tracks', by_range('captured_at', INTERVAL '1 day'));
CREATE INDEX econspicuity_tracks_icao24_idx ON econspicuity_tracks (icao24, captured_at);
ALTER TABLE econspicuity_tracks SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'icao24',
    timescaledb.compress_orderby = 'captured_at'
);
SELECT add_compression_policy('econspicuity_tracks', INTERVAL '7 days');
SELECT add_retention_policy('econspicuity_tracks', INTERVAL '90 days');

-- Own network-identification receivers: deferred. The table exists so
-- the record reads need no change when a receiver arrives; nothing
-- writes it until then.
CREATE TABLE broadcast_tracks (
    uas_id         text        NOT NULL,
    captured_at    timestamptz NOT NULL,
    ts             timestamptz NOT NULL,
    rx_ts          timestamptz NOT NULL,
    geom           geometry(Point, 4326) NOT NULL,
    alt_pressure_m double precision,
    alt_wgs84_m    double precision,
    height_m       double precision,
    height_ref     text,
    speed_ms       double precision,
    track_deg      double precision CHECK (track_deg >= 0 AND track_deg < 360),
    vspeed_ms      double precision,
    trust          text        NOT NULL CHECK (trust = 'broadcast'),
    receiver_id    text        NOT NULL,
    cell5          text        NOT NULL,
    CHECK ((height_m IS NULL) = (height_ref IS NULL))
);
SELECT create_hypertable('broadcast_tracks', by_range('captured_at', INTERVAL '1 day'));
CREATE INDEX broadcast_tracks_uas_idx ON broadcast_tracks (uas_id, captured_at);
ALTER TABLE broadcast_tracks SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'uas_id',
    timescaledb.compress_orderby = 'captured_at'
);
SELECT add_compression_policy('broadcast_tracks', INTERVAL '7 days');
SELECT add_retention_policy('broadcast_tracks', INTERVAL '90 days');

-- What a client was shown, sampled at 0.1 Hz (traffic-ws), for the record.
CREATE TABLE traffic_products (
    client_id      text        NOT NULL,
    at             timestamptz NOT NULL,
    intent_id      uuid,
    bbox           geometry(Polygon, 4326),
    tracks_shown   jsonb       NOT NULL,
    degraded       text[]      NOT NULL DEFAULT '{}',
    policy_version bigint      NOT NULL
);
SELECT create_hypertable('traffic_products', by_range('at', INTERVAL '1 day'));
CREATE INDEX traffic_products_client_idx ON traffic_products (client_id, at);
ALTER TABLE traffic_products SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'client_id',
    timescaledb.compress_orderby = 'at'
);
SELECT add_compression_policy('traffic_products', INTERVAL '7 days');
SELECT add_retention_policy('traffic_products', INTERVAL '90 days');

-- The per-sample conformance outcome, for records; judged in AMSL.
CREATE TABLE conformance_samples (
    flight_id          uuid        NOT NULL,
    captured_at        timestamptz NOT NULL,
    state              text        NOT NULL,
    distance_outside_m double precision,
    height_over_m      double precision
);
SELECT create_hypertable('conformance_samples', by_range('captured_at', INTERVAL '1 day'));
CREATE INDEX conformance_samples_flight_idx ON conformance_samples (flight_id, captured_at);
ALTER TABLE conformance_samples SET (
    timescaledb.compress,
    timescaledb.compress_segmentby = 'flight_id',
    timescaledb.compress_orderby = 'captured_at'
);
SELECT add_compression_policy('conformance_samples', INTERVAL '7 days');
SELECT add_retention_policy('conformance_samples', INTERVAL '90 days');

-- +goose Down
-- Dropping a hypertable removes its compression and retention jobs.
DROP TABLE conformance_samples;
DROP TABLE traffic_products;
DROP TABLE broadcast_tracks;
DROP TABLE econspicuity_tracks;
DROP TABLE manned_tracks;
DROP TABLE peer_flights;
DROP TABLE telemetry;
