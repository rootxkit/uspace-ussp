-- Relational tree: operational intents, their versions, flights, the
-- conformance timeline and alerts (docs/PLAN.md §5.1).
--
-- Units and datums are in the column names (CLAUDE.md rule 11). No AGL
-- is stored (D-02): an intent's volumes are kept as F3548 gave them
-- (W84) in `volumes`, with the AMSL limits derived beside them in
-- `volumes_amsl`. intent_versions and conformance_states are append-only
-- for ussp_app (SELECT and INSERT; 06 T7); nothing references them.

-- +goose Up
-- One row per intent. id is the DSS entity id. dss_state takes the four
-- F3548 OperationalIntentState values of the pinned standard
-- (uspace-core f3548 types.gen.go); it is NULL before the DSS has the
-- intent. local_state is this USSP's own lifecycle.
CREATE TABLE operational_intents (
    id                        uuid        PRIMARY KEY,
    operator_id               uuid        NOT NULL REFERENCES operator_accounts (id),
    client_id                 text        NOT NULL REFERENCES oauth_clients (client_id),
    uas_serial                text        NOT NULL,
    pilot_ref                 text,
    priority                  integer     NOT NULL DEFAULT 0,
    dss_state                 text        CHECK (dss_state IN ('Accepted', 'Activated', 'Nonconforming', 'Contingent')),
    local_state               text        NOT NULL CHECK (local_state IN (
                                  'pending_validation', 'pending_dss', 'pending_authority', 'accepted', 'activated',
                                  'nonconforming', 'contingent', 'ended', 'rejected', 'withdrawn')),
    volumes                   jsonb       NOT NULL,
    off_nominal_volumes       jsonb       NOT NULL DEFAULT '[]'::jsonb,
    volumes_amsl              jsonb       NOT NULL,
    envelope_geom             geography(Geometry, 4326) NOT NULL,
    time_start                timestamptz NOT NULL,
    time_end                  timestamptz NOT NULL,
    -- Annex IV block.
    mode                      text,
    flight_type               text,
    category                  text,
    class_label               text,
    type_certificate          text,
    identification_technology text,
    connectivity_methods      text[]      NOT NULL DEFAULT '{}',
    endurance_s               integer     CHECK (endurance_s > 0),
    loss_of_c2_procedure      text,
    operator_reg              text,
    ua_registration           text,
    contingency               jsonb,
    emergency_contact_ref     text,
    authorisation_ref         text,
    client_ref                text,
    in_uspace_airspace        boolean,
    uspace_airspace_ids       text[]      NOT NULL DEFAULT '{}',
    exempt_art_1_3            boolean     NOT NULL DEFAULT false,
    dss_ovn                   text,
    dss_version               bigint,
    decision                  text,
    authorisation_number      text        UNIQUE,
    deviation_thresholds      jsonb,
    alternative               jsonb,
    conflicts                 jsonb,
    conditions                text[]      NOT NULL DEFAULT '{}',
    cis_version_checked       bigint,
    registry_checked_at       timestamptz,
    weather_checked_ref       text,
    policy_version            bigint,
    created_at                timestamptz NOT NULL DEFAULT now(),
    updated_at                timestamptz NOT NULL DEFAULT now(),
    CHECK (time_end > time_start)
);
COMMENT ON COLUMN operational_intents.volumes IS 'F3548 Volume4D[] verbatim (W84 altitudes as given)';
COMMENT ON COLUMN operational_intents.volumes_amsl IS 'per volume lower_amsl_m, upper_amsl_m, undulation_m, derived from volumes';
COMMENT ON COLUMN operational_intents.deviation_thresholds IS '{h_m, v_m, t_s}';
CREATE INDEX operational_intents_envelope_idx ON operational_intents USING gist (envelope_geom);
CREATE INDEX operational_intents_time_idx ON operational_intents (time_start, time_end);
CREATE INDEX operational_intents_operator_idx ON operational_intents (operator_id, created_at);
-- Idempotency: one intent per client_ref and client (NULLs never collide).
CREATE UNIQUE INDEX operational_intents_client_ref_idx ON operational_intents (client_id, client_ref);

-- Every prior version of an intent: what was decided on which inputs
-- (Art. 6(6), 10(10)). Append-only.
CREATE TABLE intent_versions (
    intent_id     uuid        NOT NULL REFERENCES operational_intents (id),
    version       integer     NOT NULL CHECK (version >= 1),
    at            timestamptz NOT NULL DEFAULT now(),
    actor         text        NOT NULL,
    change_reason text        NOT NULL,
    snapshot      jsonb       NOT NULL,
    PRIMARY KEY (intent_id, version)
);

-- One flight per activated intent or per telemetry session (intent_id
-- is NULL for a session without an intent outside U-space airspace).
CREATE TABLE flights (
    id                   uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    intent_id            uuid        REFERENCES operational_intents (id),
    authorisation_number text,
    uas_serial           text        NOT NULL,
    operator_reg         text,
    client_id            text        REFERENCES oauth_clients (client_id),
    started_at           timestamptz NOT NULL,
    ended_at             timestamptz,
    end_reason           text        CHECK (end_reason IN ('landed', 'telemetry_lost', 'intent_ended', 'operator_ended')),
    rid_flight_id        text,
    isa_id               text,
    isa_version          text,
    emergency            boolean     NOT NULL DEFAULT false,
    last_state           text,
    CHECK (ended_at IS NULL OR ended_at >= started_at),
    CHECK ((ended_at IS NULL) = (end_reason IS NULL))
);
CREATE INDEX flights_intent_idx ON flights (intent_id);
CREATE INDEX flights_started_idx ON flights (started_at);
CREATE INDEX flights_open_idx ON flights (uas_serial) WHERE ended_at IS NULL;

-- The conformance timeline of a flight. Append-only: a later fact (a
-- notification sent, an acknowledgement) is a new row, never an update.
-- distance_outside_m and height_over_m are the excess outside the
-- authorised volumes, judged in AMSL (D-01); height_over_m is a vertical
-- distance above the volume's upper limit, not a height above ground.
CREATE TABLE conformance_states (
    id                 bigserial   PRIMARY KEY,
    flight_id          uuid        NOT NULL REFERENCES flights (id),
    at                 timestamptz NOT NULL,
    state              text        NOT NULL CHECK (state IN ('conforming', 'nonconforming', 'contingent', 'lost_link', 'unknown')),
    reason             text,
    distance_outside_m double precision,
    height_over_m      double precision,
    time_outside_s     double precision,
    policy_version     bigint      NOT NULL,
    peer_notified_at   timestamptz,
    ats_notified_at    timestamptz,
    ats_ack_ref        text,
    nearby_notified    jsonb
);
CREATE INDEX conformance_states_flight_idx ON conformance_states (flight_id, at);

-- Alerts of the kinds of spec 04 §3.3; policy_version travels on every
-- alert (CLAUDE.md rule 5).
CREATE TABLE alerts (
    id                   uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    kind                 text        NOT NULL,
    flight_id            uuid        REFERENCES flights (id),
    intent_id            uuid        REFERENCES operational_intents (id),
    authorisation_number text,
    peer_ref             text,
    severity             text        NOT NULL,
    state                text        NOT NULL,
    raised_at            timestamptz NOT NULL,
    updated_at           timestamptz NOT NULL,
    cleared_at           timestamptz,
    clear_reason         text,
    detail               jsonb       NOT NULL DEFAULT '{}'::jsonb,
    captured_at          timestamptz,
    policy_version       bigint      NOT NULL,
    acked_at             timestamptz,
    acked_by             text,
    escalated_at         timestamptz,
    delivery             jsonb       NOT NULL DEFAULT '{}'::jsonb,
    CHECK ((cleared_at IS NULL) = (clear_reason IS NULL))
);
CREATE INDEX alerts_flight_idx ON alerts (flight_id, raised_at);
CREATE INDEX alerts_open_idx ON alerts (raised_at) WHERE cleared_at IS NULL;

GRANT SELECT, INSERT, UPDATE, DELETE ON operational_intents, flights, alerts TO ussp_app;
GRANT SELECT, INSERT ON intent_versions, conformance_states TO ussp_app;
GRANT USAGE, SELECT ON SEQUENCE conformance_states_id_seq TO ussp_app;

-- +goose Down
DROP TABLE alerts;
DROP TABLE conformance_states;
DROP TABLE flights;
DROP TABLE intent_versions;
DROP TABLE operational_intents;
