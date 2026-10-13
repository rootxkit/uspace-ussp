-- Relational tree: the restrictions in force from the ANSP's degraded
-- direct delivery (spec 02 F2 failure rule, cross-plan M4, M5; system
-- audit 2026-10-05 H-2; internal/cis/direct.go).
--
-- cis_direct_restrictions: one row per restriction identifier, the
-- restriction/direct/v1 body the ANSP signed (and its X-JWS-Signature),
-- kept while the CISP does not hold that ansp_version, so a restart
-- keeps a restriction the CISP never published. A row is replaced only
-- by a higher ansp_version (the upsert's WHERE), and deleted once the
-- CISP holds the version or the keep period after its end has passed.
-- stored_at is the database clock.

-- +goose Up
CREATE TABLE cis_direct_restrictions (
    identifier     text        PRIMARY KEY CHECK (length(identifier) BETWEEN 1 AND 128),
    restriction_id text        NOT NULL CHECK (length(restriction_id) BETWEEN 1 AND 128),
    ansp_ref       text        NOT NULL CHECK (length(ansp_ref) BETWEEN 1 AND 256),
    ansp_version   bigint      NOT NULL CHECK (ansp_version >= 1),
    state          text        NOT NULL CHECK (state IN ('planned', 'active', 'ended', 'cancelled')),
    body           bytea       NOT NULL CHECK (octet_length(body) <= 262144),
    signature      text        NOT NULL CHECK (length(signature) <= 16384),
    issuer         text        NOT NULL CHECK (length(issuer) <= 2048),
    stored_at      timestamptz NOT NULL DEFAULT now()
);

GRANT SELECT, INSERT, UPDATE, DELETE ON cis_direct_restrictions TO ussp_app;

-- +goose Down
DROP TABLE cis_direct_restrictions;
