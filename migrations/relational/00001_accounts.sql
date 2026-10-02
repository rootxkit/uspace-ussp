-- Relational tree (PostgreSQL 16 + PostGIS 3.4), database ussp_relational.
-- goose version table: goose_db_version_relational (docs/PLAN.md D5).
-- Never run against ussp_timeseries; the two trees are never merged.
--
-- Roles. ussp_api is the login role that owns the database and runs this
-- tree (`ussp-api migrate`). ussp_app is the application role the api
-- process works as (internal/store sets it on every connection): it is
-- granted, table by table at the end of each migration, exactly what the
-- application may do. The append-only tables (events, intent_versions,
-- conformance_states) get SELECT and INSERT only (docs/PLAN.md §8, spec
-- 06 T7). Both roles are created by deploy/compose/initdb/10-ussp.sql as
-- the superuser; ussp_api cannot create roles, so this migration checks
-- and names the script instead of creating them.
--
-- The append-only grants are revoked from ussp_app, never from the owner:
-- PostgreSQL runs a foreign-key check as the owner of the referenced
-- table, so an owner without UPDATE breaks every foreign key into it
-- (42501). No foreign key references an append-only table either.

-- +goose Up
-- +goose StatementBegin
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'ussp_app') THEN
        RAISE EXCEPTION 'role ussp_app does not exist: deploy/compose/initdb/10-ussp.sql creates it as the superuser before the first migration';
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'postgis') THEN
        RAISE EXCEPTION 'extension postgis is not installed in this database: deploy/compose/initdb/10-ussp.sql creates it as the superuser';
    END IF;
END $$;
-- +goose StatementEnd

GRANT USAGE ON SCHEMA public TO ussp_app;
-- WaitForVersion reads the version as ussp_app (internal/store).
GRANT SELECT ON goose_db_version_relational TO ussp_app;

-- The USSP's own customer record; registry truth stays at the authority
-- (CLAUDE.md rule 8: the USSP's own customer record is allowed).
CREATE TABLE operator_accounts (
    id                            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    authority_registration_number text        NOT NULL UNIQUE,
    display_name                  text        NOT NULL,
    contact_email                 text        NOT NULL,
    status                        text        NOT NULL,
    validated_at                  timestamptz,
    validation_status             text,
    created_at                    timestamptz NOT NULL DEFAULT now()
);

-- OAuth2 client-credentials clients of operators (D9). secret_hash is
-- argon2id; the scopes are the catalogue of docs/PLAN.md §5.1.
CREATE TABLE oauth_clients (
    client_id   text        PRIMARY KEY,
    operator_id uuid        NOT NULL REFERENCES operator_accounts (id),
    secret_hash text        NOT NULL,
    scopes      text[]      NOT NULL DEFAULT '{}'
                CHECK (scopes <@ ARRAY['ussp.intents', 'ussp.telemetry', 'ussp.traffic', 'ussp.geo']::text[]),
    status      text        NOT NULL,
    created_at  timestamptz NOT NULL DEFAULT now(),
    rotated_at  timestamptz
);
CREATE INDEX oauth_clients_operator_idx ON oauth_clients (operator_id);

-- A client sends telemetry only for its bound serials (06 T3).
-- serial_fold is uspace-core serial.FoldKey of serial. Projected to KV
-- client_bindings.
CREATE TABLE client_serial_bindings (
    id          bigserial   PRIMARY KEY,
    client_id   text        NOT NULL REFERENCES oauth_clients (client_id),
    serial      text        NOT NULL,
    serial_fold text        NOT NULL,
    bound_at    timestamptz NOT NULL DEFAULT now(),
    unbound_at  timestamptz,
    CHECK (unbound_at IS NULL OR unbound_at >= bound_at)
);
CREATE UNIQUE INDEX client_serial_bindings_live_idx
    ON client_serial_bindings (client_id, serial_fold) WHERE unbound_at IS NULL;
CREATE INDEX client_serial_bindings_fold_idx ON client_serial_bindings (serial_fold);

-- Console users.
CREATE TABLE staff_accounts (
    id             uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    username       text        NOT NULL UNIQUE,
    password_hash  text        NOT NULL,
    role           text        NOT NULL CHECK (role IN ('supervisor', 'support', 'admin')),
    mfa_secret_ref text,
    status         text        NOT NULL,
    created_at     timestamptz NOT NULL DEFAULT now()
);

GRANT SELECT, INSERT, UPDATE, DELETE ON operator_accounts, oauth_clients, client_serial_bindings, staff_accounts TO ussp_app;
GRANT USAGE, SELECT ON SEQUENCE client_serial_bindings_id_seq TO ussp_app;

-- +goose Down
DROP TABLE staff_accounts;
DROP TABLE client_serial_bindings;
DROP TABLE oauth_clients;
DROP TABLE operator_accounts;
REVOKE ALL ON goose_db_version_relational FROM ussp_app;
REVOKE USAGE ON SCHEMA public FROM ussp_app;
-- The roles and the extension stay: they belong to the bootstrap script.
