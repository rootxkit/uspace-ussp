-- Operational intents (WP-7, internal/intent/pgstore). Every time a
-- decision rests on is the database clock; every list is bounded.

-- name: IntentNow :one
SELECT now()::timestamptz AS now;

-- name: IntentLockedNow :one
-- The database clock as it reads now, not when the transaction began:
-- inside the intents lock it ranks first come, first served (a request
-- that began first but locked second is second in line).
SELECT clock_timestamp()::timestamptz AS now;

-- name: IntentOwner :one
SELECT c.client_id, c.operator_id, c.status AS client_status, o.status AS operator_status,
       o.authority_registration_number
  FROM oauth_clients c JOIN operator_accounts o ON o.id = c.operator_id
 WHERE c.client_id = sqlc.arg(client_id);

-- name: IntentSerialBound :one
SELECT EXISTS (SELECT 1 FROM client_serial_bindings
                WHERE client_id = sqlc.arg(client_id) AND serial_fold = sqlc.arg(serial_fold) AND unbound_at IS NULL) AS bound;

-- name: IntentByClientRef :one
SELECT id, operator_id, client_id, client_ref, request_hash, request, decision_body, version, local_state,
       exempt_art_1_3, priority, time_start, time_end, filed_at, created_at, volumes_amsl, cell_set
  FROM operational_intents
 WHERE client_id = sqlc.arg(client_id) AND client_ref = sqlc.arg(client_ref);

-- name: IntentByID :one
SELECT id, operator_id, client_id, client_ref, request_hash, request, decision_body, version, local_state,
       exempt_art_1_3, priority, time_start, time_end, filed_at, created_at, volumes_amsl, cell_set
  FROM operational_intents
 WHERE id = sqlc.arg(id);

-- name: IntentForUpdate :one
SELECT id, operator_id, client_id, client_ref, request_hash, request, decision_body, version, local_state,
       exempt_art_1_3, priority, time_start, time_end, filed_at, created_at, volumes_amsl, cell_set
  FROM operational_intents
 WHERE id = sqlc.arg(id)
   FOR UPDATE;

-- name: IntentList :many
-- An operator's intents, newest first; from and to bound the window
-- (an intent is listed when its window overlaps them), state filters.
SELECT id, operator_id, client_id, client_ref, request_hash, request, decision_body, version, local_state,
       exempt_art_1_3, priority, time_start, time_end, filed_at, created_at, volumes_amsl, cell_set
  FROM operational_intents
 WHERE operator_id = sqlc.arg(operator_id)
   AND (sqlc.narg(from_at)::timestamptz IS NULL OR time_end >= sqlc.narg(from_at)::timestamptz)
   AND (sqlc.narg(to_at)::timestamptz IS NULL OR time_start <= sqlc.narg(to_at)::timestamptz)
   AND (sqlc.narg(state)::text IS NULL OR local_state = sqlc.narg(state)::text)
 ORDER BY created_at DESC, id
 LIMIT sqlc.arg(max_rows);

-- name: IntentOverlapping :many
-- The active, non-exempt intents whose envelope is within dist_m of the
-- envelope given (a MULTIPOLYGON in WKT, WGS84) and whose window
-- overlaps [from_at, to_at], without exclude_id: a prefilter, the
-- judgement is internal/intent/deconflict's.
SELECT id, operator_id, client_id, client_ref, request_hash, request, decision_body, version, local_state,
       exempt_art_1_3, priority, time_start, time_end, filed_at, created_at, volumes_amsl, cell_set
  FROM operational_intents
 WHERE local_state IN ('accepted', 'activated', 'nonconforming', 'contingent')
   AND NOT exempt_art_1_3
   AND time_start <= sqlc.arg(to_at)::timestamptz AND time_end >= sqlc.arg(from_at)::timestamptz
   AND id <> sqlc.arg(exclude_id)::uuid
   AND ST_DWithin(envelope_geom, ST_GeogFromText(sqlc.arg(envelope_wkt)::text), sqlc.arg(dist_m)::float8)
 ORDER BY filed_at, id
 LIMIT sqlc.arg(max_rows);

-- name: IntentPeerOverlapping :many
-- The peers' intents fetched after since in a DSS state whose window
-- overlaps [from_at, to_at] (a peer marked unavailable keeps its
-- intents until time_end, spec 02 F6).
SELECT entity_id, details, fetched_at
  FROM peer_intents
 WHERE fetched_at > sqlc.arg(since)::timestamptz
   AND (state IS NULL OR state IN ('Accepted', 'Activated', 'Nonconforming', 'Contingent'))
   AND time_start <= sqlc.arg(to_at)::timestamptz AND time_end >= sqlc.arg(from_at)::timestamptz
 ORDER BY fetched_at, entity_id
 LIMIT sqlc.arg(max_rows);

-- name: IntentCountOpen :one
SELECT count(*)::int AS open
  FROM operational_intents
 WHERE operator_id = sqlc.arg(operator_id)
   AND local_state IN ('pending_validation', 'pending_dss', 'pending_authority', 'accepted', 'activated', 'nonconforming', 'contingent');

-- name: IntentInsert :exec
INSERT INTO operational_intents (
    id, operator_id, client_id, uas_serial, pilot_ref, priority, dss_state, local_state, volumes, volumes_amsl,
    envelope_geom, time_start, time_end, mode, flight_type, category, subcategory, class_label, type_certificate,
    privately_built, mtom_kg, identification_technology, connectivity_methods, endurance_s, loss_of_c2_procedure,
    operator_reg, ua_registration, contingency, emergency_contact_ref, authorisation_ref, client_ref,
    in_uspace_airspace, uspace_airspace_ids, exempt_art_1_3, decision, authorisation_number, deviation_thresholds,
    alternative, conflicts, conditions, cis_version_checked, registry_checked_at, policy_version,
    version, request, request_hash, decision_body, filed_at, cell_set, created_at, updated_at)
VALUES (
    sqlc.arg(id), sqlc.arg(operator_id), sqlc.arg(client_id), sqlc.arg(uas_serial), sqlc.narg(pilot_ref),
    sqlc.arg(priority), sqlc.narg(dss_state), sqlc.arg(local_state), sqlc.arg(volumes), sqlc.arg(volumes_amsl),
    ST_GeogFromText(sqlc.arg(envelope_wkt)::text), sqlc.arg(time_start), sqlc.arg(time_end), sqlc.arg(mode),
    sqlc.arg(flight_type), sqlc.arg(category), sqlc.narg(subcategory), sqlc.narg(class_label),
    sqlc.narg(type_certificate), sqlc.arg(privately_built), sqlc.narg(mtom_kg), sqlc.arg(identification_technology),
    sqlc.arg(connectivity_methods), sqlc.arg(endurance_s), sqlc.arg(loss_of_c2_procedure), sqlc.arg(operator_reg),
    sqlc.narg(ua_registration), sqlc.arg(contingency), sqlc.arg(emergency_contact_ref), sqlc.narg(authorisation_ref),
    sqlc.arg(client_ref), sqlc.arg(in_uspace_airspace), sqlc.arg(uspace_airspace_ids), sqlc.arg(exempt_art_1_3),
    sqlc.arg(decision), sqlc.narg(authorisation_number), sqlc.narg(deviation_thresholds), sqlc.narg(alternative),
    sqlc.arg(conflicts), sqlc.arg(conditions), sqlc.narg(cis_version_checked), sqlc.narg(registry_checked_at),
    sqlc.arg(policy_version), sqlc.arg(version), sqlc.arg(request), sqlc.arg(request_hash), sqlc.arg(decision_body),
    sqlc.arg(filed_at), sqlc.arg(cell_set), sqlc.arg(created_at), sqlc.arg(created_at));

-- name: IntentUpdate :execrows
-- A new version: the state, the decision and, after a modification,
-- the volumes and what is derived from them.
UPDATE operational_intents SET
    priority = sqlc.arg(priority), dss_state = sqlc.narg(dss_state), local_state = sqlc.arg(local_state),
    volumes = sqlc.arg(volumes), volumes_amsl = sqlc.arg(volumes_amsl),
    envelope_geom = ST_GeogFromText(sqlc.arg(envelope_wkt)::text),
    time_start = sqlc.arg(time_start), time_end = sqlc.arg(time_end), in_uspace_airspace = sqlc.arg(in_uspace_airspace),
    uspace_airspace_ids = sqlc.arg(uspace_airspace_ids), exempt_art_1_3 = sqlc.arg(exempt_art_1_3),
    decision = sqlc.arg(decision), authorisation_number = sqlc.narg(authorisation_number),
    deviation_thresholds = sqlc.narg(deviation_thresholds), conflicts = sqlc.arg(conflicts),
    conditions = sqlc.arg(conditions), cis_version_checked = sqlc.narg(cis_version_checked),
    registry_checked_at = sqlc.narg(registry_checked_at), policy_version = sqlc.arg(policy_version),
    version = sqlc.arg(version), request = sqlc.arg(request), decision_body = sqlc.arg(decision_body),
    filed_at = sqlc.arg(filed_at), cell_set = sqlc.arg(cell_set), updated_at = sqlc.arg(updated_at)
 WHERE id = sqlc.arg(id) AND version = sqlc.arg(version) - 1;

-- name: IntentVersionInsert :exec
INSERT INTO intent_versions (intent_id, version, at, actor, change_reason, snapshot)
VALUES (sqlc.arg(intent_id), sqlc.arg(version), sqlc.arg(at), sqlc.arg(actor), sqlc.arg(change_reason), sqlc.arg(snapshot));

-- name: IntentFlagUpdate :execrows
-- Records that a later intent with precedence overlaps these
-- authorisations (Art. 10(10)); WP-12 updates or withdraws them.
UPDATE operational_intents
   SET update_required = sqlc.arg(update_required)
 WHERE id = ANY (sqlc.arg(ids)::uuid[])
   AND local_state IN ('accepted', 'activated', 'nonconforming', 'contingent');

-- name: IntentDueToEnd :many
-- The open intents whose time_end has passed, oldest first, locked.
SELECT id, operator_id, client_id, client_ref, request_hash, request, decision_body, version, local_state,
       exempt_art_1_3, priority, time_start, time_end, filed_at, created_at, volumes_amsl, cell_set
  FROM operational_intents
 WHERE time_end < sqlc.arg(now_at)::timestamptz
   AND local_state IN ('pending_validation', 'pending_dss', 'pending_authority', 'accepted', 'activated', 'nonconforming', 'contingent')
 ORDER BY time_end, id
 LIMIT sqlc.arg(max_rows)
   FOR UPDATE SKIP LOCKED;
