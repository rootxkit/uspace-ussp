-- Source switches (internal/sources).

-- name: SourceControlEpoch :one
SELECT epoch FROM source_control_epoch;

-- name: NextSourceControlVersion :one
SELECT nextval('source_control_version_seq')::bigint AS version;

-- name: UpsertSourceControl :one
INSERT INTO source_controls (source_type, instance_id, enabled, reason, actor, version, epoch)
VALUES (sqlc.arg(source_type), sqlc.narg(instance_id), sqlc.arg(enabled), sqlc.arg(reason), sqlc.arg(actor), sqlc.arg(version), sqlc.arg(epoch))
ON CONFLICT (source_type, instance_id) DO UPDATE
SET enabled = EXCLUDED.enabled,
    reason = EXCLUDED.reason,
    actor = EXCLUDED.actor,
    changed_at = now(),
    version = EXCLUDED.version,
    epoch = EXCLUDED.epoch
RETURNING id, source_type, instance_id, enabled, reason, actor, changed_at, version, epoch;

-- name: ListSourceControls :many
SELECT id, source_type, instance_id, enabled, reason, actor, changed_at, version, epoch
FROM source_controls
ORDER BY source_type, instance_id NULLS FIRST;
