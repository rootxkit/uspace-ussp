-- The audit log. internal/store Audit is the only caller of
-- InsertEvent (a test greps for it).

-- name: EnsureEventsPartition :one
SELECT events_ensure_partition(now())::text AS partition_name;

-- name: InsertEvent :one
INSERT INTO events (actor_type, actor_id, purpose, entity_type, entity_id, event_type, payload)
VALUES (sqlc.arg(actor_type), sqlc.arg(actor_id), sqlc.narg(purpose), sqlc.arg(entity_type), sqlc.narg(entity_id), sqlc.arg(event_type), sqlc.arg(payload))
RETURNING id, ts;

-- name: ListEventsByEntity :many
SELECT id, ts, actor_type, actor_id, purpose, entity_type, entity_id, event_type, payload
FROM events
WHERE entity_type = sqlc.arg(entity_type) AND entity_id = sqlc.arg(entity_id)::text
ORDER BY ts, id;
