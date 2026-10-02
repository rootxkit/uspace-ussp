-- uspace-ussp database bootstrap, run once by the timescaledb
-- container's entrypoint (mounted into /docker-entrypoint-initdb.d/) or
-- by CI with psql as the superuser. One container, two databases
-- (reconciliation M37); the two migration trees stay separate
-- (CLAUDE.md rule 10):
--
--   ussp_relational  PostgreSQL + PostGIS, owned by ussp_api, the only
--                    writer (migrations/relational, WP-1). ussp_api runs
--                    the migrations; the api process works as ussp_app
--                    (NOLOGIN, granted to ussp_api), which the migrations
--                    grant table by table and which may only SELECT and
--                    INSERT on the append-only tables (PLAN §8, 06 T7).
--   ussp_timeseries  TimescaleDB + PostGIS (geometry columns), owned by
--                    ussp_tsdb, the only writer (migrations/timeseries,
--                    WP-1); ussp_api reads it for records through
--                    default privileges.
--
-- The passwords come from the environment (psql \getenv), never from
-- this file: USSP_PG_API_PASSWORD and USSP_PG_TSDB_PASSWORD.

\set ON_ERROR_STOP on

\getenv api_password USSP_PG_API_PASSWORD
\getenv tsdb_password USSP_PG_TSDB_PASSWORD

\if :{?api_password}
\else
  DO $$ BEGIN RAISE EXCEPTION 'USSP_PG_API_PASSWORD is not set'; END $$;
\endif
\if :{?tsdb_password}
\else
  DO $$ BEGIN RAISE EXCEPTION 'USSP_PG_TSDB_PASSWORD is not set'; END $$;
\endif
SELECT length(:'api_password') = 0 OR length(:'tsdb_password') = 0 AS empty_password \gset
\if :empty_password
  DO $$ BEGIN RAISE EXCEPTION 'USSP_PG_API_PASSWORD and USSP_PG_TSDB_PASSWORD must not be empty'; END $$;
\endif

CREATE ROLE ussp_api LOGIN PASSWORD :'api_password';
CREATE ROLE ussp_tsdb LOGIN PASSWORD :'tsdb_password';
CREATE ROLE ussp_app NOLOGIN;
GRANT ussp_app TO ussp_api;

CREATE DATABASE ussp_relational OWNER ussp_api;
CREATE DATABASE ussp_timeseries OWNER ussp_tsdb;
REVOKE ALL ON DATABASE ussp_relational FROM PUBLIC;
REVOKE ALL ON DATABASE ussp_timeseries FROM PUBLIC;

-- Relational: PostgreSQL + PostGIS (the extension is superuser-only).
\connect ussp_relational
CREATE EXTENSION IF NOT EXISTS postgis;

-- Time series: TimescaleDB; api reads it, never writes it.
\connect ussp_timeseries
CREATE EXTENSION IF NOT EXISTS timescaledb;
CREATE EXTENSION IF NOT EXISTS postgis;
GRANT CONNECT ON DATABASE ussp_timeseries TO ussp_api;
GRANT USAGE ON SCHEMA public TO ussp_api;
ALTER DEFAULT PRIVILEGES FOR ROLE ussp_tsdb IN SCHEMA public GRANT SELECT ON TABLES TO ussp_api;
