-- Relational tree: Annex V coordination with the ANSP (WP-15; spec 02
-- F13, 04 §3.5; Reg. (EU) 2021/664 Art. 13(2), Annex V; cross-plan M2).
--
-- coordination_checks   one row per intent once api has judged, on the
--                       CIS version named, whether its volumes touch a
--                       U-space airspace in controlled airspace
--                       (in_controlled_airspace of cis_current; a
--                       requirement block that does not say, or an
--                       airspace the CIS no longer holds, counts as
--                       controlled: the ANSP is told rather than not).
--                       Written with the intent_notice it implies in one
--                       transaction, so a restart neither repeats nor
--                       loses it.
-- coordination_notices  every notice to POST {ANSP}/v1/coordination/notices,
--                       queued in the transaction that decides it and
--                       sent after its commit (the outbox). payload is
--                       the coordination/annex_v/v1 body as built, sent
--                       byte for byte on every try: the ANSP answers a
--                       repeat of a notice_ref with the first receipt
--                       (200) and the same ref with another body 409. One
--                       intent's notices go out in order. state:
--                         pending       not yet received by the ANSP
--                         received      the ANSP's receipt (ack_id) is
--                                       stored; informational kinds end
--                                       here
--                         acknowledged  a person at the ANSP acknowledged
--                                       it (acknowledged_by is a role)
--                         escalated     no acknowledgement within the
--                                       policy's ats_ack_escalate_s; shown
--                                       on the console with its age and
--                                       still polled
--                         failed        refused for good (409, a 4xx the
--                                       ANSP will refuse again) or out of
--                                       tries; shown on the console
--                       received_at, acknowledged_at and escalated_at
--                       are the database clock at the moment api read
--                       the answer; the ANSP's own times travel in
--                       ansp_received_at and ansp_acknowledged_at.
--
-- conformance_states keeps its append-only rule for every column but
-- the two the notice writes once (ats_notified_at, ats_ack_ref: spec 02
-- F13 "an acknowledgement id that the USSP records"), and gains the
-- flight's last position as the monitor reported it with the state, for
-- the nonconformance notice (04 §3.5).

-- +goose Up
ALTER TABLE conformance_states
    ADD COLUMN last_lat_deg double precision CHECK (last_lat_deg BETWEEN -90 AND 90),
    ADD COLUMN last_lng_deg double precision CHECK (last_lng_deg BETWEEN -180 AND 180),
    ADD CONSTRAINT conformance_states_last_position_check CHECK ((last_lat_deg IS NULL) = (last_lng_deg IS NULL));
GRANT UPDATE (ats_notified_at, ats_ack_ref) ON conformance_states TO ussp_app;
-- The Notifier reads the recent deviations every second.
CREATE INDEX conformance_states_deviation_idx ON conformance_states (at)
    WHERE state IN ('nonconforming', 'lost_link', 'contingent');

CREATE TABLE coordination_checks (
    intent_id     uuid        PRIMARY KEY REFERENCES operational_intents (id),
    controlled    boolean     NOT NULL,
    airspace_ids  text[]      NOT NULL DEFAULT '{}',
    unstated_ids  text[]      NOT NULL DEFAULT '{}',
    cis_version   text,
    checked_at    timestamptz NOT NULL DEFAULT now()
);
COMMENT ON COLUMN coordination_checks.airspace_ids IS 'the U-space airspaces of the intent judged controlled (or unstated, or no longer held)';
COMMENT ON COLUMN coordination_checks.unstated_ids IS 'of those, the ones whose in_controlled_airspace the CIS does not state or no longer holds';

CREATE TABLE coordination_notices (
    id                   bigserial   PRIMARY KEY,
    notice_ref           text        NOT NULL UNIQUE CHECK (length(notice_ref) BETWEEN 1 AND 128),
    kind                 text        NOT NULL CHECK (kind IN ('intent_notice', 'nonconformance', 'contingent', 'ended')),
    intent_id            uuid        NOT NULL REFERENCES operational_intents (id),
    flight_id            uuid        REFERENCES flights (id),
    conformance_state_id bigint      UNIQUE REFERENCES conformance_states (id),
    payload              jsonb       NOT NULL,
    body                 bytea       NOT NULL,
    created_at           timestamptz NOT NULL DEFAULT now(),
    state                text        NOT NULL DEFAULT 'pending'
                                     CHECK (state IN ('pending', 'received', 'acknowledged', 'escalated', 'failed')),
    attempts             integer     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_at              timestamptz NOT NULL DEFAULT now(),
    last_error           text,
    ack_id               text,
    received_at          timestamptz,
    ansp_received_at     timestamptz,
    ack_required         boolean     NOT NULL,
    polls                integer     NOT NULL DEFAULT 0 CHECK (polls >= 0),
    poll_next_at         timestamptz,
    acknowledged_at      timestamptz,
    ansp_acknowledged_at timestamptz,
    acknowledged_by      text,
    escalated_at         timestamptz,
    failed_at            timestamptz,
    CHECK ((ack_id IS NULL) = (received_at IS NULL)),
    CHECK (state = 'pending' OR state = 'failed' OR ack_id IS NOT NULL),
    CHECK ((state = 'failed') = (failed_at IS NOT NULL)),
    CHECK (state <> 'acknowledged' OR acknowledged_at IS NOT NULL),
    CHECK (kind IN ('nonconformance', 'contingent') OR conformance_state_id IS NULL)
);
COMMENT ON COLUMN coordination_notices.body IS 'the exact bytes POSTed; every retry sends these';
CREATE INDEX coordination_notices_due_idx ON coordination_notices (next_at, id) WHERE state = 'pending';
CREATE INDEX coordination_notices_intent_idx ON coordination_notices (intent_id, id);
CREATE INDEX coordination_notices_poll_idx ON coordination_notices (poll_next_at)
    WHERE ack_required AND state IN ('received', 'escalated');
CREATE INDEX coordination_notices_open_idx ON coordination_notices (created_at)
    WHERE state IN ('pending', 'escalated', 'failed');

GRANT SELECT, INSERT, UPDATE ON coordination_checks, coordination_notices TO ussp_app;
GRANT USAGE, SELECT ON SEQUENCE coordination_notices_id_seq TO ussp_app;

-- +goose Down
DROP TABLE coordination_notices;
DROP TABLE coordination_checks;
DROP INDEX conformance_states_deviation_idx;
REVOKE UPDATE (ats_notified_at, ats_ack_ref) ON conformance_states FROM ussp_app;
ALTER TABLE conformance_states
    DROP CONSTRAINT conformance_states_last_position_check,
    DROP COLUMN last_lng_deg,
    DROP COLUMN last_lat_deg;
