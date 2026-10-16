-- Relational tree: the feed cursor each cached registry answer was
-- fetched under (WP-5, internal/registry).
--
-- fetched_since is the change feed's cursor (registry_feed.since, the
-- authority's next_since) read before the answer was fetched. It
-- decides whether a change the feed applies later removes the answer:
--
--   A cached answer survives a change to its key iff it was fetched
--   under a feed cursor at or beyond the change's seq; every other
--   answer the change names is invalidated. Time (the change's at,
--   fetched_at) never decides retention.
--
-- An answer fetched under a cursor at or beyond the change was fetched
-- after this USSP received the page naming the change, which the
-- authority served after the change committed: it reflects the change
-- by causality, with no clock compared across the two systems. A row
-- written before this column existed has 0, the oldest cursor, so the
-- next change to its key invalidates it, as every change did before.

--
-- registry_validity is live by now, so its CHECK is added NOT VALID
-- (no scan under the ALTER's lock; new and updated rows are checked).
-- Every existing row takes the default 0, which satisfies it.

-- +goose Up
ALTER TABLE registry_validity ADD COLUMN fetched_since bigint NOT NULL DEFAULT 0;
ALTER TABLE registry_validity
    ADD CONSTRAINT registry_validity_fetched_since_check CHECK (fetched_since >= 0) NOT VALID;
COMMENT ON COLUMN registry_validity.fetched_since IS
    'the change feed cursor the answer was fetched under; a change with seq above it invalidates the answer';

-- +goose Down
ALTER TABLE registry_validity DROP CONSTRAINT registry_validity_fetched_since_check;
ALTER TABLE registry_validity DROP COLUMN fetched_since;
