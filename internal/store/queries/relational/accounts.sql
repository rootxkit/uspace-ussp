-- Accounts, clients, serial bindings, sign-in lockouts and sessions
-- (WP-2). internal/accounts is the only caller.

-- name: InsertOperator :one
INSERT INTO operator_accounts (authority_registration_number, display_name, contact_email, status, validation_status, validated_at)
VALUES (sqlc.arg(registration_number), sqlc.arg(display_name), sqlc.arg(contact_email), sqlc.arg(status),
        sqlc.narg(validation_status), sqlc.narg(validated_at))
RETURNING *;

-- name: OperatorByID :one
SELECT * FROM operator_accounts WHERE id = sqlc.arg(id);

-- name: OperatorByRegistration :one
SELECT * FROM operator_accounts WHERE authority_registration_number = sqlc.arg(registration_number);

-- name: UpdateOperatorContact :one
UPDATE operator_accounts
SET display_name  = COALESCE(sqlc.narg(display_name), display_name),
    contact_email = COALESCE(sqlc.narg(contact_email), contact_email)
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: SetOperatorValidation :one
UPDATE operator_accounts
SET status = sqlc.arg(status), validation_status = sqlc.narg(validation_status), validated_at = sqlc.narg(validated_at)
WHERE id = sqlc.arg(id)
RETURNING *;

-- name: InsertPortalUser :one
INSERT INTO portal_users (operator_id, username, password_hash, role, status)
VALUES (sqlc.arg(operator_id), sqlc.arg(username), sqlc.arg(password_hash), sqlc.arg(role), sqlc.arg(status))
RETURNING *;

-- name: PortalUserByUsername :one
SELECT * FROM portal_users WHERE username = sqlc.arg(username);

-- name: PortalUserByID :one
SELECT * FROM portal_users WHERE id = sqlc.arg(id);

-- name: InsertStaff :one
INSERT INTO staff_accounts (username, password_hash, role, mfa_secret_ref, status)
VALUES (sqlc.arg(username), sqlc.arg(password_hash), sqlc.arg(role), sqlc.narg(mfa_secret_ref), sqlc.arg(status))
RETURNING *;

-- name: StaffByUsername :one
SELECT * FROM staff_accounts WHERE username = sqlc.arg(username);

-- name: StaffByID :one
SELECT * FROM staff_accounts WHERE id = sqlc.arg(id);

-- name: StaffForUpdate :one
SELECT * FROM staff_accounts WHERE id = sqlc.arg(id) FOR UPDATE;

-- name: SetStaffMFAStep :exec
UPDATE staff_accounts SET mfa_last_step = sqlc.arg(step) WHERE id = sqlc.arg(id);

-- name: InsertClient :one
INSERT INTO oauth_clients (client_id, operator_id, secret_hash, scopes, status)
VALUES (sqlc.arg(client_id), sqlc.arg(operator_id), sqlc.arg(secret_hash), sqlc.arg(scopes), sqlc.arg(status))
RETURNING *;

-- name: ClientByID :one
SELECT * FROM oauth_clients WHERE client_id = sqlc.arg(client_id);

-- name: ClientForUpdate :one
SELECT * FROM oauth_clients WHERE client_id = sqlc.arg(client_id) FOR UPDATE;

-- name: ClientForToken :one
SELECT c.client_id, c.operator_id, c.secret_hash, c.previous_secret_hash, c.previous_valid_until, c.scopes,
       c.status AS client_status, o.status AS operator_status
FROM oauth_clients c JOIN operator_accounts o ON o.id = c.operator_id
WHERE c.client_id = sqlc.arg(client_id);

-- name: RotateClientSecret :one
UPDATE oauth_clients
SET secret_hash = sqlc.arg(secret_hash), previous_secret_hash = sqlc.narg(previous_secret_hash),
    previous_valid_until = sqlc.narg(previous_valid_until), rotated_at = sqlc.arg(rotated_at)
WHERE client_id = sqlc.arg(client_id)
RETURNING *;

-- name: LiveBindingByFold :one
SELECT * FROM client_serial_bindings WHERE serial_fold = sqlc.arg(serial_fold) AND unbound_at IS NULL;

-- name: InsertBinding :one
-- bound_at comes from the same clock as unbound_at, so the CHECK
-- unbound_at >= bound_at cannot trip on a skew between api and the
-- database.
INSERT INTO client_serial_bindings (client_id, serial, serial_fold, bound_at)
VALUES (sqlc.arg(client_id), sqlc.arg(serial), sqlc.arg(serial_fold), sqlc.arg(bound_at))
RETURNING *;

-- name: UnbindSerial :execrows
UPDATE client_serial_bindings SET unbound_at = sqlc.arg(at)
WHERE client_id = sqlc.arg(client_id) AND serial_fold = sqlc.arg(serial_fold) AND unbound_at IS NULL;

-- name: LiveFoldsOfClient :many
SELECT serial_fold FROM client_serial_bindings
WHERE client_id = sqlc.arg(client_id) AND unbound_at IS NULL
ORDER BY serial_fold;

-- name: LockClientBindings :exec
SELECT pg_advisory_xact_lock(hashtextextended('client_bindings:' || sqlc.arg(client_id)::text, 0));

-- name: LockoutByUsername :one
SELECT * FROM login_lockouts WHERE realm = sqlc.arg(realm) AND username = sqlc.arg(username);

-- name: EnsureLockout :exec
INSERT INTO login_lockouts (realm, username) VALUES (sqlc.arg(realm), sqlc.arg(username))
ON CONFLICT (realm, username) DO NOTHING;

-- name: LockoutForUpdate :one
SELECT * FROM login_lockouts WHERE realm = sqlc.arg(realm) AND username = sqlc.arg(username) FOR UPDATE;

-- name: SetLockout :exec
UPDATE login_lockouts SET failures = sqlc.arg(failures), locked_until = sqlc.narg(locked_until), updated_at = sqlc.arg(at)
WHERE realm = sqlc.arg(realm) AND username = sqlc.arg(username);

-- name: ClearLockout :exec
DELETE FROM login_lockouts WHERE realm = sqlc.arg(realm) AND username = sqlc.arg(username);

-- name: SweepLockouts :execrows
DELETE FROM login_lockouts
WHERE updated_at < sqlc.arg(before) AND (locked_until IS NULL OR locked_until < sqlc.arg(now));

-- name: InsertSession :exec
INSERT INTO sessions (jti, realm, account_id, roles, issued_at, expires_at, last_seen_at, remote_ip)
VALUES (sqlc.arg(jti), sqlc.arg(realm), sqlc.arg(account_id), sqlc.arg(roles), sqlc.arg(issued_at), sqlc.arg(expires_at),
        sqlc.arg(issued_at), sqlc.arg(remote_ip));

-- name: SessionByJTI :one
SELECT * FROM sessions WHERE jti = sqlc.arg(jti);

-- name: TouchSession :exec
UPDATE sessions SET last_seen_at = sqlc.arg(at) WHERE jti = sqlc.arg(jti) AND revoked_at IS NULL;

-- name: RevokeSession :execrows
UPDATE sessions SET revoked_at = sqlc.arg(at), revoke_reason = sqlc.arg(reason)
WHERE jti = sqlc.arg(jti) AND revoked_at IS NULL;

-- name: SweepSessions :execrows
DELETE FROM sessions WHERE expires_at < sqlc.arg(before);

-- name: ClientsOfOperator :many
-- The operator's clients for its portal (WP-17), oldest first, at most
-- max_rows; never a secret or its hash.
SELECT client_id, scopes, status, created_at, rotated_at, previous_valid_until
FROM oauth_clients
WHERE operator_id = sqlc.arg(operator_id)
ORDER BY created_at, client_id
LIMIT sqlc.arg(max_rows);

-- name: LiveBindingsOfClients :many
-- The live serial bindings of the clients, at most per_client of each
-- (oldest first), for the portal's client list (WP-17).
SELECT client_id, serial, bound_at
FROM (SELECT b.client_id, b.serial, b.bound_at,
             row_number() OVER (PARTITION BY b.client_id ORDER BY b.bound_at, b.serial) AS n
      FROM client_serial_bindings b
      WHERE b.client_id = ANY (sqlc.arg(client_ids)::text[]) AND b.unbound_at IS NULL) live
WHERE live.n <= sqlc.arg(per_client)::bigint
ORDER BY client_id, bound_at, serial;

-- name: PutMFAChallenge :one
-- The challenge of a staff admin's password step (WP-18): one per
-- account, a new one replacing the last, expiring ttl_s later on the
-- database clock.
INSERT INTO staff_mfa_challenges (token_hash, account_id, expires_at, remote_ip)
VALUES (sqlc.arg(token_hash), sqlc.arg(account_id), now() + make_interval(secs => sqlc.arg(ttl_s)::double precision), sqlc.narg(remote_ip))
ON CONFLICT (account_id) DO UPDATE
SET token_hash = EXCLUDED.token_hash, created_at = now(), expires_at = EXCLUDED.expires_at, attempts = 0, remote_ip = EXCLUDED.remote_ip
RETURNING expires_at;

-- name: TakeMFAChallenge :one
-- A live challenge (not expired on the database clock) with one more
-- code counted against it; no row when there is none.
UPDATE staff_mfa_challenges
SET attempts = attempts + 1
WHERE token_hash = sqlc.arg(token_hash) AND expires_at > now()
RETURNING account_id, attempts, expires_at;

-- name: DeleteMFAChallenge :execrows
DELETE FROM staff_mfa_challenges WHERE token_hash = sqlc.arg(token_hash);

-- name: SweepMFAChallenges :execrows
DELETE FROM staff_mfa_challenges WHERE expires_at < now();
