-- The DSS outbox (internal/store Outbox). Idempotent by (kind,
-- entity_id, entity_version): a second Enqueue of the same change
-- returns no row. An item is due hold_s after it is queued (0: at
-- once); until then no worker's Claim takes it.

-- name: EnqueueOutbox :one
INSERT INTO dss_outbox (kind, entity_id, entity_version, payload, next_at)
VALUES (sqlc.arg(kind), sqlc.arg(entity_id), sqlc.arg(entity_version), sqlc.arg(payload),
        now() + make_interval(secs => sqlc.arg(hold_s)::double precision))
ON CONFLICT (kind, entity_id, entity_version) DO NOTHING
RETURNING id;

-- Claims up to n due items: each is leased until now + lease_s, so a
-- worker that dies leaves it to be claimed again after the lease.
-- name: ClaimOutbox :many
UPDATE dss_outbox
SET attempts = attempts + 1,
    next_at = now() + make_interval(secs => sqlc.arg(lease_s)::double precision)
WHERE id IN (
    SELECT o.id FROM dss_outbox o
    WHERE o.done_at IS NULL AND o.next_at <= now()
    ORDER BY o.next_at, o.id
    LIMIT sqlc.arg(n)
    FOR UPDATE SKIP LOCKED
)
RETURNING id, kind, entity_id, entity_version, payload, attempts, next_at, created_at, done_at, last_error;

-- name: MarkOutboxDone :execrows
UPDATE dss_outbox SET done_at = now(), last_error = NULL
WHERE id = sqlc.arg(id) AND done_at IS NULL;

-- name: MarkOutboxFailed :execrows
UPDATE dss_outbox
SET next_at = now() + make_interval(secs => sqlc.arg(backoff_s)::double precision),
    last_error = sqlc.arg(last_error)
WHERE id = sqlc.arg(id) AND done_at IS NULL;

-- name: GetOutboxItem :one
SELECT id, kind, entity_id, entity_version, payload, attempts, next_at, created_at, done_at, last_error
FROM dss_outbox WHERE id = sqlc.arg(id);
