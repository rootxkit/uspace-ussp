-- Alerts (internal/alerts, WP-11): the record of every alert/v1 the
-- monitor publishes, written by api from alrt.v1 (docs/PLAN.md §3.2),
-- the operator's acknowledgement and the escalation of an unacknowledged
-- critical alert. A row is written only for a flight the flights table
-- holds: 0 rows means the flight fact has not been recorded yet and the
-- consumer tries again. A cleared row is never reopened by a later
-- message (a republish that crossed the clear); acknowledgement and
-- escalation are never undone by a message.

-- name: RecordAlert :one
-- flight_known is false when the flights table does not hold the flight
-- yet (the consumer tries again); an alert of an intent without a
-- flight (restriction_activated before the activation, WP-12) is known
-- once its intent is. written is false for a message on a cleared
-- alert, which changes nothing.
WITH f AS (
    SELECT id FROM flights WHERE id = sqlc.narg(flight_id)::uuid
), known AS (
    SELECT CASE WHEN sqlc.narg(flight_id)::uuid IS NULL
                THEN EXISTS (SELECT 1 FROM operational_intents oi WHERE oi.id = sqlc.narg(intent_id)::uuid)
                ELSE EXISTS (SELECT 1 FROM f) END AS ok
), ins AS (
    INSERT INTO alerts (id, kind, flight_id, intent_id, authorisation_number, peer_ref, severity, state, raised_at, updated_at,
                        cleared_at, clear_reason, detail, captured_at, policy_version, cell5)
    SELECT sqlc.arg(id)::uuid, sqlc.arg(kind), (SELECT id FROM f),
           (SELECT oi.id FROM operational_intents oi WHERE oi.id = sqlc.narg(intent_id)::uuid),
           sqlc.narg(authorisation_number), sqlc.narg(peer_ref), sqlc.arg(severity), sqlc.arg(state), sqlc.arg(raised_at),
           sqlc.arg(updated_at), sqlc.narg(cleared_at), sqlc.narg(clear_reason), sqlc.arg(detail), sqlc.arg(captured_at),
           sqlc.arg(policy_version), sqlc.narg(cell5)
    FROM known WHERE known.ok
    ON CONFLICT (id) DO UPDATE
    SET severity = EXCLUDED.severity,
        state = EXCLUDED.state,
        updated_at = GREATEST(alerts.updated_at, EXCLUDED.updated_at),
        cleared_at = EXCLUDED.cleared_at,
        clear_reason = EXCLUDED.clear_reason,
        detail = EXCLUDED.detail,
        captured_at = EXCLUDED.captured_at,
        policy_version = EXCLUDED.policy_version,
        authorisation_number = COALESCE(EXCLUDED.authorisation_number, alerts.authorisation_number),
        peer_ref = COALESCE(EXCLUDED.peer_ref, alerts.peer_ref),
        cell5 = COALESCE(EXCLUDED.cell5, alerts.cell5)
    WHERE alerts.cleared_at IS NULL
    RETURNING 1
)
SELECT (SELECT ok FROM known)::bool AS flight_known, EXISTS (SELECT 1 FROM ins) AS written;

-- name: GetAlert :one
SELECT id, kind, flight_id, intent_id, authorisation_number, peer_ref, severity, state, raised_at, updated_at, cleared_at,
       clear_reason, detail, captured_at, policy_version, acked_at, acked_by, escalated_at, delivery, cell5, recorded_at
FROM alerts
WHERE id = sqlc.arg(id)::uuid;

-- name: AckAlert :one
-- The acknowledgement of an alert of one of the caller's operator's
-- flights, or of its intents for an alert without a flight
-- (restriction_activated before the activation, WP-12), on the database
-- clock; a repeat keeps the first. No row: not this operator's alert,
-- or no such alert.
UPDATE alerts a
SET acked_at = COALESCE(a.acked_at, now()),
    acked_by = COALESCE(a.acked_by, sqlc.arg(client_id)::text),
    delivery = jsonb_set(a.delivery, ARRAY[sqlc.arg(client_id)::text],
                         COALESCE(a.delivery -> sqlc.arg(client_id)::text, '{}'::jsonb)
                         || jsonb_build_object('acked_at', to_jsonb(COALESCE(a.acked_at, now()))))
FROM oauth_clients caller
WHERE a.id = sqlc.arg(id)::uuid
  AND caller.client_id = sqlc.arg(client_id)::text
  AND caller.operator_id = (
      SELECT owner.operator_id FROM oauth_clients owner
      WHERE owner.client_id = COALESCE(
          (SELECT f.client_id FROM flights f WHERE f.id = a.flight_id),
          (SELECT oi.client_id FROM operational_intents oi WHERE oi.id = a.intent_id AND a.flight_id IS NULL)))
RETURNING a.id, a.kind, a.flight_id, a.intent_id, a.authorisation_number, a.severity, a.state, a.raised_at, a.updated_at,
          a.cleared_at, a.clear_reason, a.detail, a.captured_at, a.policy_version, a.acked_at, a.acked_by, a.escalated_at, a.cell5;

-- name: AckAlertForOperator :one
-- The acknowledgement by a portal user of the operator (brief WP-17;
-- actor = operator_user:<account id>) of an alert of one of the
-- operator's flights, or of its intents (an alert without a flight, or
-- one whose flight api has not recorded yet: the alert names the intent
-- the flight flies), on the database clock; a repeat keeps the first. No
-- row: not this operator's alert, or no such alert.
UPDATE alerts a
SET acked_at = COALESCE(a.acked_at, now()),
    acked_by = COALESCE(a.acked_by, sqlc.arg(actor)::text),
    delivery = jsonb_set(a.delivery, ARRAY[sqlc.arg(actor)::text],
                         COALESCE(a.delivery -> sqlc.arg(actor)::text, '{}'::jsonb)
                         || jsonb_build_object('acked_at', to_jsonb(COALESCE(a.acked_at, now()))))
WHERE a.id = sqlc.arg(id)::uuid
  AND sqlc.arg(operator_id)::uuid = (
      SELECT owner.operator_id FROM oauth_clients owner
      WHERE owner.client_id = COALESCE(
          (SELECT f.client_id FROM flights f WHERE f.id = a.flight_id),
          (SELECT oi.client_id FROM operational_intents oi WHERE oi.id = a.intent_id)))
RETURNING a.id, a.kind, a.flight_id, a.intent_id, a.authorisation_number, a.severity, a.state, a.raised_at, a.updated_at,
          a.cleared_at, a.clear_reason, a.detail, a.captured_at, a.policy_version, a.acked_at, a.acked_by, a.escalated_at, a.cell5;

-- name: RecordAlertDelivery :execrows
-- The first time an alert was sent to a client (traffic-ws); a later
-- send keeps it.
UPDATE alerts
SET delivery = jsonb_set(delivery, ARRAY[sqlc.arg(client_id)::text],
                         COALESCE(delivery -> sqlc.arg(client_id)::text, '{}'::jsonb)
                         || jsonb_build_object('sent_at', to_jsonb(COALESCE((delivery -> sqlc.arg(client_id)::text ->> 'sent_at')::timestamptz, sqlc.arg(sent_at)::timestamptz))))
WHERE id = sqlc.arg(id)::uuid;

-- name: EscalateAlerts :many
-- Marks, on the database clock, every critical alert still open and
-- unacknowledged escalation_after_s after the earlier of its raise and
-- its first record, oldest first, at most max_rows.
UPDATE alerts a
SET escalated_at = now()
WHERE a.id IN (
    SELECT e.id FROM alerts e
    WHERE e.severity = 'critical' AND e.cleared_at IS NULL AND e.acked_at IS NULL AND e.escalated_at IS NULL
      AND LEAST(e.raised_at, e.recorded_at) <= now() - make_interval(secs => sqlc.arg(after_s)::double precision)
    ORDER BY e.raised_at
    LIMIT sqlc.arg(max_rows)
    FOR UPDATE SKIP LOCKED
)
RETURNING a.id, a.kind, a.flight_id, a.intent_id, a.authorisation_number, a.severity, a.state, a.raised_at, a.updated_at,
          a.cleared_at, a.clear_reason, a.detail, a.captured_at, a.policy_version, a.acked_at, a.acked_by, a.escalated_at, a.cell5;

-- name: OpenIntentNoticesEnded :many
-- The open restriction_activated alerts (WP-12) whose intent is over:
-- ended, or past its time_end in any state (a withdrawn intent's
-- notice stands until the window it would have flown has passed). At
-- most max_rows, oldest first.
SELECT a.id, a.kind, a.flight_id, a.intent_id, a.authorisation_number, a.severity, a.state, a.raised_at, a.updated_at,
       a.cleared_at, a.clear_reason, a.detail, a.captured_at, a.policy_version, a.acked_at, a.acked_by, a.escalated_at, a.cell5
FROM alerts a
JOIN operational_intents oi ON oi.id = a.intent_id
WHERE a.kind = 'restriction_activated' AND a.cleared_at IS NULL
  AND (oi.local_state = 'ended' OR oi.time_end < now())
ORDER BY a.raised_at
LIMIT sqlc.arg(max_rows);

-- name: OpenIntentNotices :many
-- The open restriction_activated alerts (WP-12) whose intent is not
-- over: api republishes them from the record, so a traffic-ws that
-- restarted holds them again (they are published once by the intent's
-- projection, and no monitor republishes them). At most max_rows,
-- oldest first.
SELECT a.id, a.kind, a.flight_id, a.intent_id, a.authorisation_number, a.severity, a.state, a.raised_at, a.updated_at,
       a.cleared_at, a.clear_reason, a.detail, a.captured_at, a.policy_version, a.acked_at, a.acked_by, a.escalated_at, a.cell5
FROM alerts a
JOIN operational_intents oi ON oi.id = a.intent_id
WHERE a.kind = 'restriction_activated' AND a.cleared_at IS NULL
  AND oi.local_state <> 'ended' AND oi.time_end >= now()
ORDER BY a.raised_at
LIMIT sqlc.arg(max_rows);
