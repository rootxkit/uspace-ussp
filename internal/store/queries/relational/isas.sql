-- F3411 Identification Service Areas (internal/ridsp, brief WP-9): one
-- ISA per flight in dss_isas, planned in the transaction that records
-- the flight fact, written to the DSS by the ISA worker through the
-- dss_outbox items isa_put and isa_delete. Times come from the
-- database clock.

-- name: DBNow :one
SELECT now()::timestamptz AS now;

-- name: GetFlightISA :one
SELECT isa_id, flight_id, kind, version, time_start, time_end, extents, created_at, deleted_at, last_error
FROM dss_isas
WHERE flight_id = sqlc.arg(flight_id)::uuid
ORDER BY created_at DESC
LIMIT 1;

-- name: InsertISA :execrows
INSERT INTO dss_isas (isa_id, flight_id, kind, time_start, time_end, extents)
VALUES (sqlc.arg(isa_id), sqlc.arg(flight_id)::uuid, sqlc.arg(kind), sqlc.arg(time_start), sqlc.arg(time_end), sqlc.arg(extents))
ON CONFLICT (isa_id) DO NOTHING;

-- An ISA planned as a session one becomes its flight's intent ISA.
-- name: SetISAKind :exec
UPDATE dss_isas SET kind = sqlc.arg(kind) WHERE isa_id = sqlc.arg(isa_id);

-- An ISA with its flight's end, for the worker.
-- name: GetISAForWrite :one
SELECT i.isa_id, i.flight_id, i.kind, i.version, i.time_start, i.time_end, i.extents, i.deleted_at,
       f.ended_at AS flight_ended_at
FROM dss_isas i
LEFT JOIN flights f ON f.id = i.flight_id
WHERE i.isa_id = sqlc.arg(isa_id);

-- name: SetISAWritten :exec
UPDATE dss_isas
SET version = sqlc.arg(version), time_start = sqlc.arg(time_start), time_end = sqlc.arg(time_end),
    extents = sqlc.arg(extents), last_error = NULL, refusals = 0, refused_at = NULL
WHERE isa_id = sqlc.arg(isa_id);

-- The version the DSS holds, read after a 409: only the version, so the
-- refusals in a row are kept.
-- name: SetISAVersion :exec
UPDATE dss_isas SET version = sqlc.arg(version) WHERE isa_id = sqlc.arg(isa_id);

-- name: SetISADeleted :exec
UPDATE dss_isas SET deleted_at = now(), last_error = NULL, refusals = 0, refused_at = NULL
WHERE isa_id = sqlc.arg(isa_id) AND deleted_at IS NULL;

-- One more DSS refusal of a write of the ISA, with its error; at
-- max_refusals the ISA is marked refused (given up). Returns whether it
-- is.
-- name: CountISARefusal :one
UPDATE dss_isas
SET refusals = refusals + 1, last_error = sqlc.arg(last_error),
    refused_at = CASE WHEN refusals + 1 >= sqlc.arg(max_refusals)::integer THEN now() ELSE refused_at END
WHERE isa_id = sqlc.arg(isa_id)
RETURNING (refused_at IS NOT NULL)::boolean AS given_up;

-- The ISAs given up within the last 24 h that are neither written nor
-- deleted since, and the newest of them.
-- name: RefusedISAs :one
SELECT count(*) OVER ()::bigint AS n, isa_id, COALESCE(last_error, '')::text AS last_error
FROM dss_isas
WHERE refused_at IS NOT NULL AND deleted_at IS NULL AND refused_at > now() - interval '24 hours'
ORDER BY refused_at DESC
LIMIT 1;

-- name: SetISAError :exec
UPDATE dss_isas SET last_error = sqlc.arg(last_error)
WHERE isa_id = sqlc.arg(isa_id);

-- name: SetFlightISA :exec
UPDATE flights SET isa_id = sqlc.arg(isa_id), isa_version = sqlc.narg(isa_version)
WHERE id = sqlc.arg(flight_id)::uuid;

-- The volumes and window of the intent a flight flies.
-- name: GetIntentExtent :one
SELECT volumes, time_start, time_end
FROM operational_intents
WHERE id = sqlc.arg(id)::uuid;

-- Claims up to n due items of the given kinds, as ClaimOutbox does
-- (the ISA worker takes only isa_put and isa_delete).
-- name: ClaimOutboxKinds :many
UPDATE dss_outbox
SET attempts = attempts + 1,
    next_at = now() + make_interval(secs => sqlc.arg(lease_s)::double precision)
WHERE id IN (
    SELECT o.id FROM dss_outbox o
    WHERE o.done_at IS NULL AND o.next_at <= now() AND o.kind = ANY (sqlc.arg(kinds)::text[])
    ORDER BY o.next_at, o.id
    LIMIT sqlc.arg(n)
    FOR UPDATE SKIP LOCKED
)
RETURNING id, kind, entity_id, entity_version, payload, attempts, next_at, created_at, done_at, last_error;

-- The undone items of the given kinds and the age of the oldest.
-- name: OutboxBacklog :one
SELECT count(*)::bigint AS pending,
       COALESCE(EXTRACT(EPOCH FROM now() - min(created_at)), 0)::double precision AS oldest_age_s
FROM dss_outbox
WHERE done_at IS NULL AND kind = ANY (sqlc.arg(kinds)::text[]);

-- Session ISAs of flights still going on whose time_end is within
-- before_s and that no isa_put waits for: due for renewal (one item per
-- ISA at a time, so a long DSS outage queues nothing more).
-- name: SessionISAsDue :many
SELECT i.isa_id, i.flight_id, i.version, i.time_end, i.extents
FROM dss_isas i
JOIN flights f ON f.id = i.flight_id
WHERE i.deleted_at IS NULL AND i.version IS NOT NULL AND f.ended_at IS NULL
  AND i.kind = 'session'
  AND NOT EXISTS (
      SELECT 1 FROM dss_outbox o
      WHERE o.kind = 'isa_put' AND o.entity_id = i.isa_id AND o.done_at IS NULL
  )
  AND i.time_end < now() + make_interval(secs => sqlc.arg(before_s)::double precision)
ORDER BY i.time_end
LIMIT sqlc.arg(n);
