-- Relational tree: occurrence reports, operating status notices,
-- weather products and the daily record bundles (docs/PLAN.md §5.1).

-- +goose Up
-- deadline_at is became_aware_at + 72 h, written by internal/occurrence;
-- undelivered reports are on the console.
CREATE TABLE occurrence_reports (
    id              uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    report_ref      text        NOT NULL UNIQUE,
    kind            text        NOT NULL,
    occurred_at     timestamptz,
    became_aware_at timestamptz NOT NULL,
    deadline_at     timestamptz NOT NULL,
    payload         jsonb       NOT NULL,
    submitted_at    timestamptz,
    authority_ref   text,
    attempts        integer     NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    last_error      text,
    CHECK (deadline_at > became_aware_at)
);
CREATE INDEX occurrence_reports_undelivered_idx ON occurrence_reports (deadline_at) WHERE submitted_at IS NULL;

-- Art. 7(6) start / cease / restart notices.
CREATE TABLE operating_status_notices (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    kind          text        NOT NULL CHECK (kind IN ('start', 'cease', 'restart')),
    at            timestamptz NOT NULL,
    submitted_at  timestamptz,
    authority_ref text
);

-- Optional Art. 12 products.
CREATE TABLE weather_products (
    id          uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    area        geography(Geometry, 4326) NOT NULL,
    observed_at timestamptz,
    valid_from  timestamptz NOT NULL,
    valid_to    timestamptz NOT NULL,
    source      text        NOT NULL,
    product     jsonb       NOT NULL,
    fetched_at  timestamptz NOT NULL,
    CHECK (valid_to > valid_from)
);
CREATE INDEX weather_products_area_idx ON weather_products USING gist (area);

-- Daily bundles for GET /v1/records/daily/{date}; flights is the number
-- of flight records in the bundle.
CREATE TABLE record_bundles (
    date         date        PRIMARY KEY,
    built_at     timestamptz NOT NULL,
    content_hash text        NOT NULL,
    storage_ref  text        NOT NULL,
    flights      integer     NOT NULL CHECK (flights >= 0)
);

GRANT SELECT, INSERT, UPDATE, DELETE ON occurrence_reports, operating_status_notices, weather_products, record_bundles TO ussp_app;

-- +goose Down
DROP TABLE record_bundles;
DROP TABLE weather_products;
DROP TABLE operating_status_notices;
DROP TABLE occurrence_reports;
