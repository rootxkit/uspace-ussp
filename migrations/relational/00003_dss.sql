-- Relational tree: the DSS side (F3411 ISAs, subscriptions, the outbox
-- dss-sync works through, the USS availability) and the peer data the
-- DSS leads to (docs/PLAN.md §5.1).
--
-- peer_intents is purged at 24 h (uspace-core f3548
-- ExternalDataMaxRetentionTimeHours, a standard limit, not policy)
-- except what a decision record copied.

-- +goose Up
CREATE TABLE dss_isas (
    isa_id     text        PRIMARY KEY,
    flight_id  uuid        REFERENCES flights (id),
    version    text,
    time_start timestamptz NOT NULL,
    time_end   timestamptz NOT NULL,
    extents    jsonb       NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    deleted_at timestamptz,
    last_error text,
    CHECK (time_end > time_start)
);
CREATE INDEX dss_isas_flight_idx ON dss_isas (flight_id);

CREATE TABLE dss_subscriptions (
    subscription_id    text        PRIMARY KEY,
    kind               text        NOT NULL CHECK (kind IN ('rid', 'utm')),
    area               jsonb       NOT NULL,
    version            text,
    notification_index integer     NOT NULL DEFAULT 0 CHECK (notification_index >= 0),
    time_end           timestamptz NOT NULL,
    uss_base_url       text        NOT NULL,
    renewed_at         timestamptz
);

-- Work for dss-sync: idempotent by (kind, entity_id, entity_version), so
-- enqueueing the same change twice queues it once. internal/store Outbox
-- is the only writer.
CREATE TABLE dss_outbox (
    id             bigserial   PRIMARY KEY,
    kind           text        NOT NULL CHECK (kind IN ('oir_put', 'oir_delete', 'isa_put', 'isa_delete', 'peer_notify', 'uss_report')),
    entity_id      text        NOT NULL,
    entity_version bigint      NOT NULL DEFAULT 0,
    payload        jsonb       NOT NULL,
    attempts       integer     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_at        timestamptz NOT NULL DEFAULT now(),
    created_at     timestamptz NOT NULL DEFAULT now(),
    done_at        timestamptz,
    last_error     text,
    UNIQUE (kind, entity_id, entity_version)
);
CREATE INDEX dss_outbox_due_idx ON dss_outbox (next_at, id) WHERE done_at IS NULL;

-- Singleton: this USSP's availability as the DSS knows it (F3548
-- UssAvailabilityState of the pinned standard) and the DSS reachability.
CREATE TABLE dss_state (
    singleton             boolean     PRIMARY KEY DEFAULT true CHECK (singleton),
    uss_availability      text        CHECK (uss_availability IN ('Unknown', 'Normal', 'Down')),
    set_by                text,
    set_at                timestamptz,
    dss_reachable_since   timestamptz,
    dss_unreachable_since timestamptz
);
INSERT INTO dss_state (singleton) VALUES (true);

CREATE TABLE peer_intents (
    entity_id        text        PRIMARY KEY,
    manager          text        NOT NULL,
    uss_base_url     text        NOT NULL,
    state            text,
    ovn              text,
    version          bigint,
    time_start       timestamptz,
    time_end         timestamptz,
    details          jsonb,
    fetched_at       timestamptz NOT NULL,
    peer_unavailable boolean     NOT NULL DEFAULT false
);
CREATE INDEX peer_intents_fetched_idx ON peer_intents (fetched_at);

-- F3548 constraint intake; cis_restriction_id joins to the CIS
-- restriction when the ANSP reference matches.
CREATE TABLE constraints (
    entity_id          text        PRIMARY KEY,
    manager            text        NOT NULL,
    ovn                text,
    version            bigint,
    time_start         timestamptz,
    time_end           timestamptz,
    details            jsonb,
    cis_restriction_id text,
    fetched_at         timestamptz NOT NULL
);
CREATE INDEX constraints_fetched_idx ON constraints (fetched_at);

GRANT SELECT, INSERT, UPDATE, DELETE ON dss_isas, dss_subscriptions, dss_outbox, dss_state, peer_intents, constraints TO ussp_app;
GRANT USAGE, SELECT ON SEQUENCE dss_outbox_id_seq TO ussp_app;

-- +goose Down
DROP TABLE constraints;
DROP TABLE peer_intents;
DROP TABLE dss_state;
DROP TABLE dss_outbox;
DROP TABLE dss_subscriptions;
DROP TABLE dss_isas;
