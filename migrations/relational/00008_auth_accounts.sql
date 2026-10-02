-- Relational tree: accounts, clients and sessions of WP-2 (docs/PLAN.md
-- §5.1, §8; brief WP-2).
--
-- portal_users: the people of an operator who sign in to the portal
-- (roles operator_admin, remote_pilot, viewer). The username is the
-- USSP's own customer record (CLAUDE.md rule 8).
--
-- oauth_clients gains the previous secret of a rotation and the end of
-- its overlap (policy.client_secret_overlap_s).
--
-- staff_accounts gains the last TOTP time step accepted, so a code is
-- accepted once (RFC 6238 §5.2) on every replica.
--
-- login_lockouts: the consecutive failed sign-ins per realm and
-- username, and the lock they set. In the database, not in a process,
-- so a lock holds across api replicas (authority WP-2 review lesson);
-- keyed by the username typed, whether or not an account has it, so a
-- lock does not tell an unknown username from a known one. Rows that
-- are not locked and untouched for a day are swept (E-10).
--
-- sessions: one row per session token (jti = session id): logout and
-- the idle end revoke it, and every request checks it, on every replica.
-- Rows expired for a day are swept (E-10).
--
-- client_serial_bindings: a serial is live-bound to one client at most.

-- +goose Up
ALTER TABLE operator_accounts
    ADD CONSTRAINT operator_accounts_status_check CHECK (status IN ('pending_validation', 'active', 'refused'));

CREATE TABLE portal_users (
    id            uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    operator_id   uuid        NOT NULL REFERENCES operator_accounts (id),
    username      text        NOT NULL UNIQUE CHECK (username <> ''),
    password_hash text        NOT NULL,
    role          text        NOT NULL CHECK (role IN ('operator_admin', 'remote_pilot', 'viewer')),
    status        text        NOT NULL CHECK (status IN ('active', 'disabled')),
    created_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX portal_users_operator_idx ON portal_users (operator_id);

ALTER TABLE oauth_clients
    ADD COLUMN previous_secret_hash text,
    ADD COLUMN previous_valid_until timestamptz,
    ADD CONSTRAINT oauth_clients_status_check CHECK (status IN ('active', 'disabled'));

ALTER TABLE staff_accounts
    ADD COLUMN mfa_last_step bigint NOT NULL DEFAULT 0,
    ADD CONSTRAINT staff_accounts_status_check CHECK (status IN ('active', 'disabled'));

CREATE TABLE login_lockouts (
    realm        text        NOT NULL CHECK (realm IN ('portal', 'console')),
    username     text        NOT NULL,
    failures     integer     NOT NULL DEFAULT 0 CHECK (failures >= 0),
    locked_until timestamptz,
    updated_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (realm, username)
);

CREATE TABLE sessions (
    jti           text        PRIMARY KEY,
    realm         text        NOT NULL CHECK (realm IN ('portal', 'console')),
    account_id    uuid        NOT NULL,
    roles         text[]      NOT NULL,
    issued_at     timestamptz NOT NULL,
    expires_at    timestamptz NOT NULL,
    last_seen_at  timestamptz NOT NULL,
    revoked_at    timestamptz,
    revoke_reason text,
    remote_ip     text        NOT NULL DEFAULT ''
);
CREATE INDEX sessions_account_idx ON sessions (account_id);
CREATE INDEX sessions_expires_idx ON sessions (expires_at);

CREATE UNIQUE INDEX client_serial_bindings_one_live_idx
    ON client_serial_bindings (serial_fold) WHERE unbound_at IS NULL;

GRANT SELECT, INSERT, UPDATE, DELETE ON portal_users, login_lockouts, sessions TO ussp_app;

-- +goose Down
DROP INDEX client_serial_bindings_one_live_idx;
DROP TABLE sessions;
DROP TABLE login_lockouts;
ALTER TABLE staff_accounts DROP CONSTRAINT staff_accounts_status_check, DROP COLUMN mfa_last_step;
ALTER TABLE oauth_clients DROP CONSTRAINT oauth_clients_status_check, DROP COLUMN previous_valid_until, DROP COLUMN previous_secret_hash;
DROP TABLE portal_users;
ALTER TABLE operator_accounts DROP CONSTRAINT operator_accounts_status_check;
