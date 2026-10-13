-- The authority's deploy/initdb/10-authority.sql: POSTGRES_DB creates
-- the relational database `authority`; this creates the telemetry
-- database `authority_ts` beside it (M37). Roles and extensions are the
-- authority's migrations'.
CREATE DATABASE authority_ts;
