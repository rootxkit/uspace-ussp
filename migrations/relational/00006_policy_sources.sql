-- Relational tree: the versioned policy row and the source switches
-- (docs/PLAN.md D10, D11, §5.1; LESSONS INV-03, B-09).
--
-- policy: one row per version, never updated (ussp_app may SELECT and
-- INSERT). Versions come from policy_version_seq, so a version number is
-- never reused, even when a transaction that took one rolled back after
-- its KV projection was written: a follower takes only a higher version.
-- The values are the JSON of internal/policy.Values; the defaults live
-- there (PLAN §15 Q6), not here.
--
-- source_controls: the current switch per source type (instance_id NULL)
-- and per instance; history is in events. version comes from
-- source_control_version_seq, epoch from the one source_control_epoch
-- row created here: a database restored elsewhere is a new epoch only
-- when this migration runs again, which is what followers key on
-- (uspace-core sources.Follower.Apply).

-- +goose Up
CREATE SEQUENCE policy_version_seq AS bigint MINVALUE 1;

CREATE TABLE policy (
    version    bigint      PRIMARY KEY CHECK (version >= 1),
    created_at timestamptz NOT NULL DEFAULT now(),
    actor      text        NOT NULL CHECK (actor <> ''),
    reason     text        NOT NULL CHECK (reason <> ''),
    "values"   jsonb       NOT NULL CHECK (jsonb_typeof("values") = 'object')
);

CREATE SEQUENCE source_control_version_seq AS bigint MINVALUE 1;

CREATE TABLE source_control_epoch (
    singleton  boolean     PRIMARY KEY DEFAULT true CHECK (singleton),
    epoch      text        NOT NULL CHECK (epoch <> ''),
    created_at timestamptz NOT NULL DEFAULT now()
);
INSERT INTO source_control_epoch (epoch) VALUES (gen_random_uuid()::text);

CREATE TABLE source_controls (
    id          bigserial   PRIMARY KEY,
    source_type text        NOT NULL CHECK (source_type <> ''),
    instance_id text        CHECK (instance_id <> ''),
    enabled     boolean     NOT NULL,
    reason      text        NOT NULL CHECK (reason <> ''),
    actor       text        NOT NULL CHECK (actor <> ''),
    changed_at  timestamptz NOT NULL DEFAULT now(),
    version     bigint      NOT NULL CHECK (version >= 1),
    epoch       text        NOT NULL,
    UNIQUE NULLS NOT DISTINCT (source_type, instance_id)
);

GRANT SELECT, INSERT ON policy TO ussp_app;
GRANT SELECT ON source_control_epoch TO ussp_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON source_controls TO ussp_app;
GRANT USAGE, SELECT ON SEQUENCE policy_version_seq, source_control_version_seq, source_controls_id_seq TO ussp_app;

-- +goose Down
DROP TABLE source_controls;
DROP TABLE source_control_epoch;
DROP SEQUENCE source_control_version_seq;
DROP TABLE policy;
DROP SEQUENCE policy_version_seq;
