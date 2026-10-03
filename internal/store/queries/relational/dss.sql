-- F3548 strategic coordination through the DSS (internal/dss, brief
-- WP-13): what the DSS holds of each intent, the peers' intents and
-- constraints, the subscriptions, this USSP's availability, the exchange
-- log and the reports. Times come from the database clock.

-- name: IntentDSSHeld :one
SELECT id, version, local_state, dss_held_state, dss_ovn, dss_version, dss_reference, dss_extents,
       dss_subscription_id, dss_written_at, dss_last_error
FROM operational_intents
WHERE id = sqlc.arg(id)::uuid;

-- What the DSS answered to a write of the intent: written only after the
-- DSS answer, never in the decision's transaction.
-- name: IntentSetDSSHeld :execrows
UPDATE operational_intents
SET dss_held_state = sqlc.arg(held_state), dss_ovn = sqlc.arg(ovn), dss_version = sqlc.arg(dss_version),
    dss_reference = sqlc.arg(reference), dss_extents = sqlc.arg(extents),
    dss_subscription_id = sqlc.arg(subscription_id), dss_written_at = now(), dss_last_error = NULL
WHERE id = sqlc.arg(id)::uuid;

-- The DSS no longer holds the intent (deleted, or never held).
-- name: IntentClearDSSHeld :execrows
UPDATE operational_intents
SET dss_held_state = NULL, dss_ovn = NULL, dss_reference = NULL, dss_extents = NULL,
    dss_subscription_id = NULL, dss_written_at = now(), dss_last_error = NULL
WHERE id = sqlc.arg(id)::uuid;

-- name: IntentSetDSSError :exec
UPDATE operational_intents SET dss_last_error = sqlc.arg(last_error)
WHERE id = sqlc.arg(id)::uuid;

-- Whether the DSS holds the intent (an OIR to delete or update).
-- name: IntentDSSIsHeld :one
SELECT EXISTS (SELECT 1 FROM operational_intents WHERE id = sqlc.arg(id)::uuid AND dss_ovn IS NOT NULL)::boolean AS held;

-- The newest flight of an intent (the telemetry of
-- GET /uss/v1/operational_intents/{id}/telemetry).
-- name: IntentNewestFlight :one
SELECT id, started_at, ended_at
FROM flights
WHERE intent_id = sqlc.arg(intent_id)::uuid
ORDER BY started_at DESC
LIMIT 1;

-- A peer's intent as its manager gave it (trust: provider), newer
-- versions only: an older version never replaces a newer one. fetched_at
-- is the database clock; peer_unavailable is cleared by an answer.
-- name: PeerIntentUpsert :execrows
INSERT INTO peer_intents (entity_id, manager, uss_base_url, state, ovn, version, time_start, time_end, details,
                          priority, fetched_at, peer_unavailable)
VALUES (sqlc.arg(entity_id), sqlc.arg(manager), sqlc.arg(uss_base_url), sqlc.arg(state), sqlc.arg(ovn),
        sqlc.arg(version), sqlc.arg(time_start), sqlc.arg(time_end), sqlc.arg(details), sqlc.arg(priority), now(), false)
ON CONFLICT (entity_id) DO UPDATE
SET manager = EXCLUDED.manager, uss_base_url = EXCLUDED.uss_base_url, state = EXCLUDED.state, ovn = EXCLUDED.ovn,
    version = EXCLUDED.version, time_start = EXCLUDED.time_start, time_end = EXCLUDED.time_end,
    details = EXCLUDED.details, priority = EXCLUDED.priority, fetched_at = now(), peer_unavailable = false
WHERE peer_intents.version IS NULL OR EXCLUDED.version >= peer_intents.version;

-- name: PeerIntentGet :one
SELECT entity_id, manager, uss_base_url, state, ovn, version, time_start, time_end, details, fetched_at,
       peer_unavailable, priority
FROM peer_intents
WHERE entity_id = sqlc.arg(entity_id);

-- A deleted reference removes the peer's intent, only its manager's.
-- name: PeerIntentDelete :execrows
DELETE FROM peer_intents WHERE entity_id = sqlc.arg(entity_id) AND manager = sqlc.arg(manager);

-- A peer that does not answer: its intents are kept (they still count
-- until time_end) and marked; an answer clears the mark.
-- name: PeerIntentsMarkUnavailable :execrows
UPDATE peer_intents SET peer_unavailable = sqlc.arg(unavailable)
WHERE uss_base_url = sqlc.arg(uss_base_url) AND peer_unavailable <> sqlc.arg(unavailable);

-- The peers' intents of a window, with their mark, for the peer view.
-- name: PeerIntentsUnavailable :many
SELECT entity_id, uss_base_url, time_end
FROM peer_intents
WHERE peer_unavailable AND time_end >= now()
ORDER BY entity_id
LIMIT sqlc.arg(max_rows);

-- Peer data older than before (F3548 ExternalDataMaxRetentionTimeHours)
-- that no decision record names: a decision copies what it relied on
-- into operational_intents.conflicts (ref "peer:<entity_id>"), and a row
-- it names is kept.
-- name: PeerIntentsPurge :execrows
DELETE FROM peer_intents p
WHERE p.fetched_at < sqlc.arg(before)::timestamptz
  AND NOT EXISTS (
      SELECT 1 FROM operational_intents o
      WHERE o.conflicts @> jsonb_build_array(jsonb_build_object('ref', 'peer:' || p.entity_id)));

-- name: ConstraintUpsert :execrows
INSERT INTO constraints (entity_id, manager, ovn, version, time_start, time_end, details, cis_restriction_id,
                         uss_base_url, fetched_at)
VALUES (sqlc.arg(entity_id), sqlc.arg(manager), sqlc.arg(ovn), sqlc.arg(version), sqlc.arg(time_start),
        sqlc.arg(time_end), sqlc.arg(details), sqlc.arg(cis_restriction_id), sqlc.arg(uss_base_url), now())
ON CONFLICT (entity_id) DO UPDATE
SET manager = EXCLUDED.manager, ovn = EXCLUDED.ovn, version = EXCLUDED.version, time_start = EXCLUDED.time_start,
    time_end = EXCLUDED.time_end, details = EXCLUDED.details, cis_restriction_id = EXCLUDED.cis_restriction_id,
    uss_base_url = EXCLUDED.uss_base_url, fetched_at = now()
WHERE constraints.version IS NULL OR EXCLUDED.version >= constraints.version;

-- name: ConstraintGet :one
SELECT entity_id, manager, ovn, version, time_start, time_end, details, cis_restriction_id, uss_base_url, fetched_at
FROM constraints
WHERE entity_id = sqlc.arg(entity_id);

-- name: ConstraintDelete :execrows
DELETE FROM constraints WHERE entity_id = sqlc.arg(entity_id) AND manager = sqlc.arg(manager);

-- The constraints whose window overlaps [from_at, to_at] fetched after
-- since, for the deconfliction of an intent written to the DSS.
-- name: ConstraintsOverlapping :many
SELECT entity_id, details, fetched_at
FROM constraints
WHERE fetched_at > sqlc.arg(since)::timestamptz
  AND time_start <= sqlc.arg(to_at)::timestamptz AND time_end >= sqlc.arg(from_at)::timestamptz
ORDER BY fetched_at, entity_id
LIMIT sqlc.arg(max_rows);

-- name: ConstraintsPurge :execrows
DELETE FROM constraints c
WHERE c.fetched_at < sqlc.arg(before)::timestamptz
  AND NOT EXISTS (
      SELECT 1 FROM operational_intents o
      WHERE o.conflicts @> jsonb_build_array(jsonb_build_object('ref', 'constraint:' || c.entity_id)));

-- name: SubscriptionUpsert :exec
INSERT INTO dss_subscriptions (subscription_id, kind, area, version, notification_index, time_end, uss_base_url, renewed_at)
VALUES (sqlc.arg(subscription_id), 'utm', sqlc.arg(area), sqlc.arg(version), sqlc.arg(notification_index),
        sqlc.arg(time_end), sqlc.arg(uss_base_url), now())
ON CONFLICT (subscription_id) DO UPDATE
SET area = EXCLUDED.area, version = EXCLUDED.version,
    notification_index = GREATEST(dss_subscriptions.notification_index, EXCLUDED.notification_index),
    time_end = EXCLUDED.time_end, uss_base_url = EXCLUDED.uss_base_url, renewed_at = now();

-- name: SubscriptionsUTM :many
SELECT subscription_id, kind, area, version, notification_index, time_end, uss_base_url, renewed_at
FROM dss_subscriptions
WHERE kind = 'utm'
ORDER BY subscription_id
LIMIT sqlc.arg(max_rows);

-- name: SubscriptionDelete :execrows
DELETE FROM dss_subscriptions WHERE subscription_id = sqlc.arg(subscription_id) AND kind = 'utm';

-- The notification index of one of our subscriptions, advanced to idx;
-- the index held before, and false when the subscription is not ours.
-- name: SubscriptionNotified :one
UPDATE dss_subscriptions s
SET notification_index = GREATEST(s.notification_index, sqlc.arg(idx)::integer)
FROM (SELECT d.subscription_id AS sid, d.notification_index AS previous
      FROM dss_subscriptions d
      WHERE d.subscription_id = sqlc.arg(subscription_id)
      FOR UPDATE) old
WHERE s.subscription_id = old.sid
RETURNING old.previous::integer AS previous;

-- name: DSSStateGet :one
SELECT uss_availability, set_by, set_at, dss_reachable_since, dss_unreachable_since
FROM dss_state WHERE singleton;

-- name: DSSStateSetAvailability :execrows
UPDATE dss_state
SET uss_availability = sqlc.arg(availability), set_by = sqlc.arg(set_by), set_at = now()
WHERE singleton AND uss_availability IS DISTINCT FROM sqlc.arg(availability);

-- name: DSSStateSetReachable :execrows
UPDATE dss_state
SET dss_reachable_since = CASE WHEN sqlc.arg(up)::boolean THEN COALESCE(dss_reachable_since, now()) ELSE NULL END,
    dss_unreachable_since = CASE WHEN sqlc.arg(up)::boolean THEN NULL ELSE COALESCE(dss_unreachable_since, now()) END
WHERE singleton;

-- name: ExchangeInsert :exec
INSERT INTO dss_exchanges (entity_id, recorder_role, method, url, request_body, request_time, response_code,
                           response_body, response_time, problem)
VALUES (sqlc.narg(entity_id), sqlc.arg(recorder_role), sqlc.arg(method), sqlc.arg(url), sqlc.narg(request_body),
        sqlc.arg(request_time), sqlc.narg(response_code), sqlc.narg(response_body), sqlc.narg(response_time),
        sqlc.narg(problem));

-- name: ExchangesByEntity :many
SELECT id, entity_id, recorder_role, method, url, request_body, request_time, response_code, response_body,
       response_time, problem
FROM dss_exchanges
WHERE entity_id = sqlc.arg(entity_id)
ORDER BY id
LIMIT sqlc.arg(max_rows);

-- name: ExchangesPurgeBefore :execrows
DELETE FROM dss_exchanges WHERE recorded_at < sqlc.arg(before)::timestamptz;

-- Keeps the newest max_rows exchanges.
-- name: ExchangesPurgeBeyond :execrows
DELETE FROM dss_exchanges
WHERE id < (SELECT e.id FROM dss_exchanges e ORDER BY e.id DESC OFFSET sqlc.arg(max_rows)::bigint LIMIT 1);

-- name: ReportInsert :exec
INSERT INTO uss_reports (report_id, reporter, exchange)
VALUES (sqlc.arg(report_id)::uuid, sqlc.arg(reporter), sqlc.arg(exchange));

-- name: ReportsPurgeBefore :execrows
DELETE FROM uss_reports WHERE received_at < sqlc.arg(before)::timestamptz;

-- name: MarkOutboxDoneByKey :execrows
UPDATE dss_outbox SET done_at = now(), last_error = NULL
WHERE kind = sqlc.arg(kind) AND entity_id = sqlc.arg(entity_id) AND entity_version = sqlc.arg(entity_version)
  AND done_at IS NULL;

-- name: OutboxOldestDue :one
SELECT count(*)::bigint AS pending,
       COALESCE(EXTRACT(EPOCH FROM now() - min(created_at)), 0)::double precision AS oldest_age_s,
       COALESCE(max(attempts), 0)::integer AS max_attempts
FROM dss_outbox
WHERE done_at IS NULL AND kind = ANY (sqlc.arg(kinds)::text[]);

-- The newest authorisation number an earlier version of the intent
-- carried (Art. 6(6): a modification keeps it).
-- name: IntentPreviousNumber :one
SELECT (snapshot->'decision'->>'authorisation_number')::text AS number
FROM intent_versions
WHERE intent_id = sqlc.arg(intent_id)::uuid AND snapshot->'decision'->>'authorisation_number' IS NOT NULL
ORDER BY version DESC
LIMIT 1;
