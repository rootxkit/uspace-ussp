-- The USSP console (internal/admin, WP-18): the reads of the console
-- pages and the console's own facts on alerts and emergency cases. Every
-- time written is the database clock; every write goes with its events
-- row in the caller's transaction (internal/store Audit).

-- name: AdminActiveFlights :many
-- The flights not ended, newest first, at most max_rows, with their
-- intent's states, the newest conformance state recorded and whether an
-- emergency case is open.
SELECT f.id, f.intent_id, f.authorisation_number, f.uas_serial, f.operator_reg, f.client_id, f.started_at, f.last_state, f.emergency,
       oi.local_state AS intent_state, oi.dss_state,
       cs.state AS conformance_state, cs.at AS conformance_at, cs.reason AS conformance_reason,
       EXISTS (SELECT 1 FROM emergency_cases ec WHERE ec.flight_id = f.id AND ec.closed_at IS NULL) AS case_open
FROM flights f
LEFT JOIN operational_intents oi ON oi.id = f.intent_id
LEFT JOIN conformance_states cs ON cs.id = (
    SELECT c.id FROM conformance_states c WHERE c.flight_id = f.id ORDER BY c.at DESC, c.id DESC LIMIT 1
)
WHERE f.ended_at IS NULL
ORDER BY f.started_at DESC, f.id
LIMIT sqlc.arg(max_rows);

-- name: AdminAlerts :many
-- The alerts not cleared, and with recent those cleared in the last
-- recent_s, newest first, at most max_rows.
SELECT id, kind, flight_id, intent_id, authorisation_number, severity, state, raised_at, updated_at, cleared_at, clear_reason,
       detail, policy_version, acked_at, acked_by, escalated_at, escalated_by, escalation_reason, closed_at, closed_by,
       close_reason, messages_recorded
FROM alerts
WHERE cleared_at IS NULL
   OR (sqlc.arg(recent)::bool AND cleared_at > now() - make_interval(secs => sqlc.arg(recent_s)::double precision))
ORDER BY raised_at DESC, id
LIMIT sqlc.arg(max_rows);

-- name: AdminEscalations :many
-- The alerts escalated and not closed on the console, oldest escalation
-- first, at most max_rows.
SELECT id, kind, flight_id, intent_id, authorisation_number, severity, state, raised_at, updated_at, cleared_at, clear_reason,
       detail, policy_version, acked_at, acked_by, escalated_at, escalated_by, escalation_reason, closed_at, closed_by,
       close_reason, messages_recorded
FROM alerts
WHERE escalated_at IS NOT NULL AND closed_at IS NULL
ORDER BY escalated_at, id
LIMIT sqlc.arg(max_rows);

-- name: AdminAlertForUpdate :one
-- One alert, locked for the console's write in this transaction.
SELECT id, kind, flight_id, intent_id, authorisation_number, severity, state, raised_at, updated_at, cleared_at, clear_reason,
       detail, captured_at, policy_version, acked_at, acked_by, escalated_at, escalated_by, escalation_reason, closed_at, closed_by,
       close_reason, messages_recorded, cell5
FROM alerts
WHERE id = sqlc.arg(id)::uuid
FOR UPDATE;

-- name: AdminEscalateAlert :one
-- A supervisor's escalation of an alert not escalated yet; no row when
-- it was (the caller answers it unchanged).
UPDATE alerts
SET escalated_at = now(), escalated_by = sqlc.arg(staff_id)::text, escalation_reason = sqlc.arg(reason)::text
WHERE id = sqlc.arg(id)::uuid AND escalated_at IS NULL
RETURNING escalated_at;

-- name: AdminCloseAlert :one
-- The console's close of an alert not closed yet; no row when it was.
UPDATE alerts
SET closed_at = now(), closed_by = sqlc.arg(staff_id)::text, close_reason = sqlc.arg(reason)::text
WHERE id = sqlc.arg(id)::uuid AND closed_at IS NULL
RETURNING closed_at;

-- name: AdminOutboxDepth :many
-- The outbox items not done, by kind, with the oldest and how many
-- failed at least once.
SELECT kind, count(*)::bigint AS pending, (count(*) FILTER (WHERE attempts > 0))::bigint AS retrying,
       min(created_at)::timestamptz AS oldest_created_at
FROM dss_outbox
WHERE done_at IS NULL
GROUP BY kind
ORDER BY kind;

-- name: AdminOutboxErrors :many
-- The newest outbox items with an error, at most max_rows.
SELECT kind, entity_id, attempts, last_error, next_at, done_at
FROM dss_outbox
WHERE last_error IS NOT NULL
ORDER BY next_at DESC, id DESC
LIMIT sqlc.arg(max_rows);

-- name: AdminSubscriptions :many
SELECT subscription_id, kind, time_end, renewed_at, notification_index
FROM dss_subscriptions
ORDER BY time_end DESC, subscription_id
LIMIT sqlc.arg(max_rows);

-- name: AdminPolicyHistory :many
-- The newest policy versions, newest first, at most max_rows.
SELECT version, created_at, actor, reason, "values"
FROM policy
ORDER BY version DESC
LIMIT sqlc.arg(max_rows);

-- name: AdminFlightForCase :one
-- A flight and its intent's emergency contact reference, for a case.
SELECT f.id, f.intent_id, f.authorisation_number, f.uas_serial, oi.emergency_contact_ref
FROM flights f
LEFT JOIN operational_intents oi ON oi.id = f.intent_id
WHERE f.id = sqlc.arg(id)::uuid;

-- name: AdminOpenCase :one
-- A new open case; no row while one is open for the flight.
INSERT INTO emergency_cases (flight_id, opened_by, reason)
VALUES (sqlc.arg(flight_id)::uuid, sqlc.arg(opened_by), sqlc.arg(reason))
ON CONFLICT (flight_id) WHERE closed_at IS NULL DO NOTHING
RETURNING id;

-- name: AdminOpenCaseOfFlight :one
-- The flight's open case, locked for a note or the close.
SELECT id FROM emergency_cases WHERE flight_id = sqlc.arg(flight_id)::uuid AND closed_at IS NULL FOR UPDATE;

-- name: AdminAddNote :one
INSERT INTO emergency_notes (case_id, author, step, text)
VALUES (sqlc.arg(case_id)::uuid, sqlc.arg(author), sqlc.narg(step), sqlc.arg(text))
RETURNING at;

-- name: AdminCloseCase :one
UPDATE emergency_cases
SET closed_at = now(), closed_by = sqlc.arg(closed_by)::text, outcome = sqlc.arg(outcome)::text
WHERE id = sqlc.arg(id)::uuid AND closed_at IS NULL
RETURNING closed_at;

-- name: AdminCaseOfFlight :one
-- The flight's newest case.
SELECT c.id, c.flight_id, c.opened_at, c.opened_by, c.reason, c.closed_at, c.closed_by, c.outcome,
       f.intent_id, f.authorisation_number, f.uas_serial, oi.emergency_contact_ref
FROM emergency_cases c
JOIN flights f ON f.id = c.flight_id
LEFT JOIN operational_intents oi ON oi.id = f.intent_id
WHERE c.flight_id = sqlc.arg(flight_id)::uuid
ORDER BY c.opened_at DESC, c.id
LIMIT 1;

-- name: AdminCases :many
-- The open cases and those closed in the last closed_s, open first,
-- newest first, at most max_rows.
SELECT c.id, c.flight_id, c.opened_at, c.opened_by, c.reason, c.closed_at, c.closed_by, c.outcome,
       f.intent_id, f.authorisation_number, f.uas_serial, oi.emergency_contact_ref
FROM emergency_cases c
JOIN flights f ON f.id = c.flight_id
LEFT JOIN operational_intents oi ON oi.id = f.intent_id
WHERE c.closed_at IS NULL OR c.closed_at > now() - make_interval(secs => sqlc.arg(closed_s)::double precision)
ORDER BY (c.closed_at IS NULL) DESC, c.opened_at DESC, c.id
LIMIT sqlc.arg(max_rows);

-- name: AdminCaseNotes :many
-- The notes of the cases, oldest first, at most max_rows.
SELECT case_id, at, author, step, text
FROM emergency_notes
WHERE case_id = ANY (sqlc.arg(case_ids)::uuid[])
ORDER BY at, id
LIMIT sqlc.arg(max_rows);

-- name: AdminRecordDays :many
-- The bundles built from first_day on, newest first.
SELECT date, built_at, content_hash, flights
FROM record_bundles
WHERE date >= sqlc.arg(first_day)::date
ORDER BY date DESC;

-- name: AdminEvents :many
-- The events rows of one console entity, oldest first, at most max_rows.
SELECT id, ts, actor_type, actor_id, entity_type, entity_id, event_type, payload
FROM events
WHERE entity_type = sqlc.arg(entity_type) AND entity_id = sqlc.arg(entity_id)::text
ORDER BY ts, id
LIMIT sqlc.arg(max_rows);
