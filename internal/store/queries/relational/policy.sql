-- The versioned policy row (internal/policy through internal/store).

-- name: NextPolicyVersion :one
SELECT nextval('policy_version_seq')::bigint AS version;

-- name: InsertPolicy :one
INSERT INTO policy (version, actor, reason, "values")
VALUES (sqlc.arg(version), sqlc.arg(actor), sqlc.arg(reason), sqlc.arg(policy_values))
RETURNING version, created_at, actor, reason, "values";

-- name: NewestPolicy :one
SELECT version, created_at, actor, reason, "values"
FROM policy ORDER BY version DESC LIMIT 1;
