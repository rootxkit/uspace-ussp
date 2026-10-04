-- Relational tree: the weather products of WP-16 (internal/weather;
-- Art. 12) and the state of the configured source.
--
-- weather_products (00005) gets the station and the kind of each report
-- and one row per report: (source, station, kind, observed_at) is
-- unique, so every api instance polling the source stores a report
-- once. observed_at is the observation of a METAR or SPECI and the
-- issue of a TAF, never unknown. Nothing wrote the table before this
-- migration.
--
-- weather_source_status keeps the outcome of the last fetch of each
-- source on the database clock: a source failing before a restart is
-- still shown failing, with its time, after it (E-02).

-- +goose Up
-- A row can predate this Up only after its Down ran (nothing else
-- wrote the table): its station and kind are read back from what the
-- row itself holds (the product's area and its raw report). A row that
-- says neither was never written by internal/weather and is removed,
-- the one case where this migration deletes anything.
ALTER TABLE weather_products ADD COLUMN station text, ADD COLUMN kind text;
UPDATE weather_products
   SET station = product -> 'area' ->> 'station',
       kind = CASE split_part(product ->> 'raw', ' ', 1) WHEN 'METAR' THEN 'metar' WHEN 'SPECI' THEN 'speci' WHEN 'TAF' THEN 'taf' END;
DELETE FROM weather_products
 WHERE station IS NULL OR kind IS NULL OR station !~ '^[A-Z][A-Z0-9]{3}$' OR observed_at IS NULL;
ALTER TABLE weather_products
    ALTER COLUMN station SET NOT NULL,
    ALTER COLUMN kind SET NOT NULL,
    ALTER COLUMN observed_at SET NOT NULL,
    ADD CONSTRAINT weather_products_station_check CHECK (station ~ '^[A-Z][A-Z0-9]{3}$'),
    ADD CONSTRAINT weather_products_kind_check CHECK (kind IN ('metar', 'speci', 'taf'));
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
