-- Operating-status notices to the authority (internal/status, WP-15;
-- Art. 7(6)). Every time is the database clock.

-- The notices of a certificate, newest first: what the next notice may
-- be follows from the newest that did not fail.
-- name: StatusNotices :many
SELECT *
FROM operating_status_notices
WHERE certificate_id = sqlc.arg(certificate_id)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(n);

-- name: StatusInsert :one
INSERT INTO operating_status_notices (kind, at, certificate_id, reference, requested_by, follows)
VALUES (sqlc.arg(kind), now(), sqlc.arg(certificate_id), sqlc.arg(reference), sqlc.arg(requested_by), sqlc.narg(follows)::uuid)
RETURNING *;

-- name: StatusClaim :many
UPDATE operating_status_notices
SET attempts = attempts + 1, next_at = now() + make_interval(secs => sqlc.arg(lease_s)::double precision)
WHERE id IN (
    SELECT n.id FROM operating_status_notices n
    WHERE n.state = 'pending' AND n.next_at <= now()
      AND NOT EXISTS (SELECT 1 FROM operating_status_notices e
                      WHERE e.certificate_id = n.certificate_id AND e.state = 'pending' AND e.created_at < n.created_at)
    ORDER BY n.created_at
    LIMIT sqlc.arg(n)
    FOR UPDATE SKIP LOCKED
)
RETURNING id, kind, at, certificate_id, reference, attempts;

-- name: StatusDelivered :one
UPDATE operating_status_notices
SET state = 'delivered', submitted_at = now(), authority_ref = sqlc.arg(authority_ref), last_error = NULL
WHERE id = sqlc.arg(id)::uuid AND state = 'pending'
RETURNING reference, kind;

-- name: StatusRetry :execrows
UPDATE operating_status_notices
SET next_at = now() + make_interval(secs => sqlc.arg(backoff_s)::double precision), last_error = sqlc.arg(last_error)
WHERE id = sqlc.arg(id)::uuid AND state = 'pending';

-- name: StatusFail :one
UPDATE operating_status_notices
SET state = 'failed', failed_at = now(), last_error = sqlc.arg(last_error)
WHERE id = sqlc.arg(id)::uuid AND state = 'pending'
RETURNING reference, kind;
