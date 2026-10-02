-- Transaction-scoped advisory locks (internal/store lock keys). Released
-- at commit or rollback.

-- name: AdvisoryXactLock :exec
SELECT pg_advisory_xact_lock(sqlc.arg(key)::bigint);

-- The lock of one entity of a class (the two-key form, whose key space
-- does not overlap the single-key locks above): the class is an
-- internal/store lock class, the entity is hashed.
-- name: AdvisoryXactLockEntity :exec
SELECT pg_advisory_xact_lock(sqlc.arg(class)::integer, hashtext(sqlc.arg(entity_id)::text));
