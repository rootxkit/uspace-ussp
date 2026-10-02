-- Transaction-scoped advisory locks (internal/store lock keys). Released
-- at commit or rollback.

-- name: AdvisoryXactLock :exec
SELECT pg_advisory_xact_lock(sqlc.arg(key)::bigint);
