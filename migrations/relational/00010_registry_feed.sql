-- Relational tree: the registry validity cache of WP-5 (internal/registry)
-- completed: the change feed's cursor and the invalidations it applied.
--
-- registry_validity.valid_until is the registration's end of validity
-- as F8 answers it (an operator's), so it is null where F8 gives none
-- (a UAS, a pilot, an unknown operator). The cache's own lifetime is
-- not stored: it is fetched_at plus the policy's TTL at the time of the
-- read, so a policy change applies to every cached answer at once.
--
-- key_fold is what a change invalidates: an operator's compare key
-- (regnum.CompareKey), a serial's fold key (serial.FoldKey, because a
-- serial may have been looked up in another case) and a pilot's id.
--
-- registry_invalidations remembers, for an hour, the keys the feed
-- invalidated and the sequence that did it: an answer fetched while
-- the feed was invalidating its key (its fetch started under an older
-- cursor) is not written, so an invalidation is never undone by a
-- write that raced it (G-08). registry_feed is one row: the cursor
-- (the authority's next_since), the ETag of its page and when the feed
-- last answered, on the database clock.

-- +goose Up
ALTER TABLE registry_validity ALTER COLUMN valid_until DROP NOT NULL;
ALTER TABLE registry_validity ADD COLUMN key_fold text;
UPDATE registry_validity SET key_fold = key;
ALTER TABLE registry_validity ALTER COLUMN key_fold SET NOT NULL;
ALTER TABLE registry_validity ADD CONSTRAINT registry_validity_entity_type_check
    CHECK (entity_type IN ('operator', 'uas', 'pilot'));
ALTER TABLE registry_validity ADD CONSTRAINT registry_validity_status_check
    CHECK (status IN ('valid', 'suspended', 'revoked', 'unknown'));
-- Every write is bounded (E-10): the keys F8 accepts are at most 64
-- characters, a class label and a band are short, and a pilot's
-- competency set is at most internal/registry.MaxCompetencies entries.
ALTER TABLE registry_validity ADD CONSTRAINT registry_validity_bounds_check
    CHECK (length(key) BETWEEN 1 AND 128 AND length(key_fold) BETWEEN 1 AND 128
           AND (class_label IS NULL OR length(class_label) <= 64)
           AND (mtom_band IS NULL OR length(mtom_band) <= 64)
           AND (competencies IS NULL OR (jsonb_typeof(competencies) = 'array' AND jsonb_array_length(competencies) <= 64)));
CREATE INDEX registry_validity_fold_idx ON registry_validity (entity_type, key_fold);
COMMENT ON COLUMN registry_validity.valid_until IS 'the registration''s end of validity as F8 answers it; null where F8 gives none';

CREATE TABLE registry_invalidations (
    entity_type text        NOT NULL CHECK (entity_type IN ('operator', 'uas', 'pilot')),
    key_fold    text        NOT NULL CHECK (length(key_fold) BETWEEN 1 AND 128),
    seq         bigint      NOT NULL CHECK (seq >= 0),
    at          timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (entity_type, key_fold)
);
CREATE INDEX registry_invalidations_at_idx ON registry_invalidations (at);

CREATE TABLE registry_feed (
    id        boolean     PRIMARY KEY DEFAULT true CHECK (id),
    since     bigint      NOT NULL CHECK (since >= 0),
    etag      text CHECK (etag IS NULL OR length(etag) <= 128),
    polled_at timestamptz NOT NULL
);

GRANT SELECT, INSERT, UPDATE, DELETE ON registry_invalidations, registry_feed TO ussp_app;

-- +goose Down
DROP TABLE registry_feed;
DROP TABLE registry_invalidations;
DROP INDEX registry_validity_fold_idx;
ALTER TABLE registry_validity DROP CONSTRAINT registry_validity_bounds_check;
ALTER TABLE registry_validity DROP CONSTRAINT registry_validity_status_check;
ALTER TABLE registry_validity DROP CONSTRAINT registry_validity_entity_type_check;
ALTER TABLE registry_validity DROP COLUMN key_fold;
DELETE FROM registry_validity WHERE valid_until IS NULL;
ALTER TABLE registry_validity ALTER COLUMN valid_until SET NOT NULL;
