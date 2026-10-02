-- Relational tree: the CIS cache and the registry validity cache
-- (docs/PLAN.md §5.1).
--
-- cis_features keeps only the current and the previous dataset version
-- (history is the CISP's). Zone limits are kept as ED-318 gives them:
-- a value with its reference (lower_m + lower_ref, upper_m + upper_ref),
-- never converted into a stored AGL (D-02). registry_validity holds
-- statuses, never PII (CLAUDE.md rule 8).

-- +goose Up
CREATE TABLE cis_datasets (
    dataset      text        NOT NULL,
    version      bigint      NOT NULL CHECK (version >= 0),
    etag         text,
    fetched_at   timestamptz NOT NULL,
    metadata     jsonb       NOT NULL DEFAULT '{}'::jsonb,
    signature_ok boolean     NOT NULL,
    PRIMARY KEY (dataset, version)
);

-- The primary key's leading columns are the (dataset, version) index.
CREATE TABLE cis_features (
    dataset         text        NOT NULL,
    version         bigint      NOT NULL,
    feature_id      text        NOT NULL,
    feature         jsonb       NOT NULL,
    geom            geography(Geometry, 4326) NOT NULL,
    lower_m         double precision,
    lower_ref       text,
    upper_m         double precision,
    upper_ref       text,
    applicable_from timestamptz,
    applicable_to   timestamptz,
    zone_type       text,
    PRIMARY KEY (dataset, version, feature_id),
    FOREIGN KEY (dataset, version) REFERENCES cis_datasets (dataset, version) ON DELETE CASCADE,
    CHECK ((lower_m IS NULL) = (lower_ref IS NULL)),
    CHECK ((upper_m IS NULL) = (upper_ref IS NULL))
);
COMMENT ON COLUMN cis_features.feature IS 'ED-318 feature verbatim';
CREATE INDEX cis_features_geom_idx ON cis_features USING gist (geom);

-- The webhook log.
CREATE TABLE cis_notifications (
    id          bigserial   PRIMARY KEY,
    received_at timestamptz NOT NULL DEFAULT now(),
    dataset     text        NOT NULL,
    version     bigint,
    feature_ids text[]      NOT NULL DEFAULT '{}',
    reason      text,
    jws_ok      boolean     NOT NULL,
    pulled_at   timestamptz
);
CREATE INDEX cis_notifications_received_idx ON cis_notifications (received_at);

-- TTL 24 h positive, 5 min negative (policy); invalidated by the F8
-- change feed. The primary key is the (entity_type, key) index.
CREATE TABLE registry_validity (
    entity_type text        NOT NULL,
    key         text        NOT NULL,
    status      text        NOT NULL,
    valid_until timestamptz NOT NULL,
    class_label text,
    mtom_band   text,
    competencies jsonb,
    fetched_at  timestamptz NOT NULL,
    negative    boolean     NOT NULL DEFAULT false,
    PRIMARY KEY (entity_type, key)
);

GRANT SELECT, INSERT, UPDATE, DELETE ON cis_datasets, cis_features, cis_notifications, registry_validity TO ussp_app;
GRANT USAGE, SELECT ON SEQUENCE cis_notifications_id_seq TO ussp_app;

-- +goose Down
DROP TABLE registry_validity;
DROP TABLE cis_notifications;
DROP TABLE cis_features;
DROP TABLE cis_datasets;
