-- Relational tree: F3548 strategic coordination through the DSS (WP-13,
-- internal/dss).
--
-- operational_intents gains what the DSS holds of the intent, written
-- only by the DSS writer after a DSS answer: the state the DSS holds
-- (dss_held_state, one of the four F3548 states, NULL when the DSS holds
-- nothing), the OperationalIntentReference it answered verbatim
-- (dss_reference: ovn, version, manager, subscription), the extents
-- written (dss_extents), the implicit subscription and when it was
-- written. dss_state keeps the state the intent's decision asks for;
-- dss_ovn and dss_version (migration 00002) are filled from the answer.
--
-- peer_intents gains the peer's priority (details.priority, kept beside
-- the details for the deconfliction), and constraints the identifier of
-- the ANSP restriction a constraint names.
--
-- dss_exchanges is the exchange log of GET /uss/v1/log_sets: every DSS
-- and peer request and answer this USSP made or served, bodies clipped,
-- bounded in rows and kept dss_exchange_retention_days (policy).
-- uss_reports keeps the reports peers POST to /uss/v1/reports.

-- +goose Up
-- dss_ovn and dss_version mean what the DSS holds only with the state
-- held beside them: a value without it (none before this migration; a
-- tree rolled back past it and up again) is cleared, and the writer
-- reads the reference from the DSS again (it adopts what it finds).
UPDATE operational_intents SET dss_ovn = NULL, dss_version = NULL WHERE dss_ovn IS NOT NULL;
ALTER TABLE operational_intents
    ADD COLUMN dss_held_state      text CHECK (dss_held_state IN ('Accepted', 'Activated', 'Nonconforming', 'Contingent')),
    ADD COLUMN dss_reference       jsonb,
    ADD COLUMN dss_extents         jsonb,
    ADD COLUMN dss_subscription_id text,
    ADD COLUMN dss_written_at      timestamptz,
    ADD COLUMN dss_last_error      text,
    ADD CONSTRAINT operational_intents_dss_held_check CHECK ((dss_held_state IS NULL) = (dss_ovn IS NULL));
CREATE INDEX operational_intents_dss_held_idx ON operational_intents (id) WHERE dss_ovn IS NOT NULL;

ALTER TABLE peer_intents
    ADD COLUMN priority integer NOT NULL DEFAULT 0;
CREATE INDEX peer_intents_base_url_idx ON peer_intents (uss_base_url);

ALTER TABLE constraints
    ADD COLUMN uss_base_url text;

CREATE TABLE dss_exchanges (
    id            bigserial   PRIMARY KEY,
    entity_id     text,
    recorder_role text        NOT NULL CHECK (recorder_role IN ('Client', 'Server')),
    method        text        NOT NULL,
    url           text        NOT NULL,
    request_body  text,
    request_time  timestamptz NOT NULL,
    response_code integer,
    response_body text,
    response_time timestamptz,
    problem       text,
    recorded_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX dss_exchanges_entity_idx ON dss_exchanges (entity_id, id);
CREATE INDEX dss_exchanges_recorded_idx ON dss_exchanges (recorded_at);

CREATE TABLE uss_reports (
    report_id   uuid        PRIMARY KEY,
    reporter    text        NOT NULL,
    exchange    jsonb       NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX uss_reports_received_idx ON uss_reports (received_at);

GRANT SELECT, INSERT, UPDATE, DELETE ON dss_exchanges, uss_reports TO ussp_app;
GRANT USAGE, SELECT ON SEQUENCE dss_exchanges_id_seq TO ussp_app;

-- +goose Down
DROP TABLE uss_reports;
DROP TABLE dss_exchanges;
ALTER TABLE constraints DROP COLUMN uss_base_url;
DROP INDEX peer_intents_base_url_idx;
ALTER TABLE peer_intents DROP COLUMN priority;
DROP INDEX operational_intents_dss_held_idx;
ALTER TABLE operational_intents
    DROP CONSTRAINT operational_intents_dss_held_check,
    DROP COLUMN dss_last_error,
    DROP COLUMN dss_written_at,
    DROP COLUMN dss_subscription_id,
    DROP COLUMN dss_extents,
    DROP COLUMN dss_reference,
    DROP COLUMN dss_held_state;
-- Without the state held beside them they mean nothing (see Up).
UPDATE operational_intents SET dss_ovn = NULL, dss_version = NULL WHERE dss_ovn IS NOT NULL;
