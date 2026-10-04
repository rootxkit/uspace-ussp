-- Relational tree: the weather products of WP-16 (internal/weather;
-- Art. 12) and the state of the configured source.
--
-- weather_products (00005) gets the station and the kind of each report
-- and one row per report: (source, station, kind, observed_at) is
-- unique, so every api instance polling the source stores a report
-- once. observed_at is the observation of a METAR or SPECI and the
-- issue of a TAF, never unknown. Nothing wrote the table before this
-- migration, so the NOT NULL columns rewrite no row.
--
-- weather_source_status keeps the outcome of the last fetch of each
-- source on the database clock: a source failing before a restart is
-- still shown failing, with its time, after it (E-02).

-- +goose Up
ALTER TABLE weather_products
    ADD COLUMN station text NOT NULL CHECK (station ~ '^[A-Z][A-Z0-9]{3}$'),
    ADD COLUMN kind    text NOT NULL CHECK (kind IN ('metar', 'speci', 'taf')),
    ALTER COLUMN observed_at SET NOT NULL;
CREATE UNIQUE INDEX weather_products_report_idx ON weather_products (source, station, kind, observed_at);
CREATE INDEX weather_products_valid_to_idx ON weather_products (valid_to);

CREATE TABLE weather_source_status (
    source          text        PRIMARY KEY,
    last_attempt_at timestamptz NOT NULL,
    last_success_at timestamptz,
    last_failure_at timestamptz,
    last_error      text        CHECK (length(last_error) <= 512)
);

GRANT SELECT, INSERT, UPDATE ON weather_source_status TO ussp_app;

-- +goose Down
DROP TABLE weather_source_status;
DROP INDEX weather_products_valid_to_idx;
DROP INDEX weather_products_report_idx;
ALTER TABLE weather_products
    ALTER COLUMN observed_at DROP NOT NULL,
    DROP COLUMN kind,
    DROP COLUMN station;
