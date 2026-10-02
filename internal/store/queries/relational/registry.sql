-- The registry validity cache of WP-5 (internal/registry): the F8
-- answers by key, the change feed's cursor and the invalidations it
-- applied. Every age and every timestamp is the database clock.

-- name: RegistryValidityByKeys :many
-- The cached answers for the (entity_type, key) pairs, with their age.
SELECT v.entity_type, v.key, v.key_fold, v.status, v.valid_until, v.class_label, v.mtom_band,
       v.competencies, v.fetched_at, v.negative,
       extract(epoch FROM now() - v.fetched_at)::float8 AS age_s
  FROM registry_validity v
  JOIN (SELECT unnest(sqlc.arg(entity_types)::text[]) AS entity_type,
               unnest(sqlc.arg(keys)::text[]) AS key) AS k
    ON v.entity_type = k.entity_type AND v.key = k.key;

-- name: RegistryValidityAll :many
-- Every cached answer, newest first, at most max_rows (the api's
-- table-backed identification lookup).
SELECT entity_type, key, key_fold, status, valid_until, class_label, mtom_band,
       competencies, fetched_at, negative,
       extract(epoch FROM now() - fetched_at)::float8 AS age_s
  FROM registry_validity
 ORDER BY fetched_at DESC
 LIMIT sqlc.arg(max_rows);

-- name: UpsertRegistryValidity :one
-- Writes one answer unless the change feed invalidated its key under a
-- sequence above since (the cursor read before the fetch): an answer
-- that raced an invalidation is never written (no row returned).
INSERT INTO registry_validity (entity_type, key, key_fold, status, valid_until, class_label, mtom_band,
                               competencies, fetched_at, negative)
SELECT sqlc.arg(entity_type)::text, sqlc.arg(key)::text, sqlc.arg(key_fold)::text, sqlc.arg(status)::text,
       sqlc.narg(valid_until)::timestamptz, sqlc.narg(class_label)::text, sqlc.narg(mtom_band)::text,
       sqlc.narg(competencies)::jsonb, now(), sqlc.arg(negative)::boolean
 WHERE NOT EXISTS (
       SELECT 1 FROM registry_invalidations i
        WHERE i.entity_type = sqlc.arg(entity_type)::text AND i.key_fold = sqlc.arg(key_fold)::text
          AND i.seq > sqlc.arg(since)::bigint)
ON CONFLICT (entity_type, key) DO UPDATE
   SET key_fold = EXCLUDED.key_fold, status = EXCLUDED.status, valid_until = EXCLUDED.valid_until,
       class_label = EXCLUDED.class_label, mtom_band = EXCLUDED.mtom_band,
       competencies = EXCLUDED.competencies, fetched_at = EXCLUDED.fetched_at, negative = EXCLUDED.negative
RETURNING fetched_at;

-- name: DeleteRegistryValidityByFold :many
-- Invalidates every cached answer under a fold key and names them.
DELETE FROM registry_validity
 WHERE entity_type = sqlc.arg(entity_type) AND key_fold = sqlc.arg(key_fold)
RETURNING entity_type, key;

-- name: RecordRegistryInvalidation :exec
INSERT INTO registry_invalidations (entity_type, key_fold, seq, at)
VALUES (sqlc.arg(entity_type), sqlc.arg(key_fold), sqlc.arg(seq), now())
ON CONFLICT (entity_type, key_fold) DO UPDATE
   SET seq = GREATEST(registry_invalidations.seq, EXCLUDED.seq), at = now();

-- name: PruneRegistryInvalidations :execrows
-- Forgets invalidations older than keep_s seconds: no fetch is in
-- flight that long (the F8 deadline is seconds).
DELETE FROM registry_invalidations WHERE at < now() - make_interval(secs => sqlc.arg(keep_s)::float8);

-- name: RegistryFeedCursor :one
-- The feed's cursor, the ETag of its page and the age of its last
-- answer.
SELECT since, etag, polled_at, extract(epoch FROM now() - polled_at)::float8 AS age_s
  FROM registry_feed
 WHERE id;

-- name: SetRegistryFeedCursor :exec
-- Moves the cursor (never backwards) and records the answer's time.
INSERT INTO registry_feed (id, since, etag, polled_at)
VALUES (true, sqlc.arg(since), sqlc.narg(etag), now())
ON CONFLICT (id) DO UPDATE
   SET since = GREATEST(registry_feed.since, EXCLUDED.since), etag = EXCLUDED.etag, polled_at = now();
