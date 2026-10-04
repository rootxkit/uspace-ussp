-- Annex V coordination with the ANSP (internal/coordination, WP-15):
-- the checks, the notices outbox and the two facts a notice writes on
-- its conformance state. Every time is the database clock.

-- The conformance states recorded within lookback that enter a deviation
-- kind (nonconforming or lost_link: nonconformance; contingent:
-- contingent) from a state of another kind (or from none), of a flight
-- of an authorised intent, without a notice yet.
-- name: CoordinationTransitions :many
SELECT cs.id, cs.flight_id, cs.at, cs.state, cs.reason, cs.distance_outside_m, cs.height_over_m,
       cs.last_lat_deg, cs.last_lng_deg,
       i.id AS intent_id, i.authorisation_number, i.local_state, i.dss_state, i.time_start, i.time_end, i.volumes
FROM conformance_states cs
JOIN flights f ON f.id = cs.flight_id
JOIN operational_intents i ON i.id = f.intent_id
WHERE cs.state IN ('nonconforming', 'lost_link', 'contingent')
  AND cs.at > now() - make_interval(secs => sqlc.arg(lookback_s)::double precision)
  AND i.authorisation_number IS NOT NULL AND NOT i.exempt_art_1_3
  AND NOT EXISTS (SELECT 1 FROM coordination_notices n WHERE n.conformance_state_id = cs.id)
  AND (CASE WHEN cs.state = 'contingent' THEN 'contingent' ELSE 'nonconformance' END) IS DISTINCT FROM (
      SELECT CASE WHEN p.state = 'contingent' THEN 'contingent'
                  WHEN p.state IN ('nonconforming', 'lost_link') THEN 'nonconformance' END
      FROM conformance_states p
      WHERE p.flight_id = cs.flight_id AND (p.at, p.id) < (cs.at, cs.id)
      ORDER BY p.at DESC, p.id DESC
      LIMIT 1)
ORDER BY cs.at, cs.id
LIMIT sqlc.arg(n);

-- The authorised intents activated (or later) without a check: the
-- active ones, and the ended ones whose versions show an activation and
-- that ended within lookback.
-- name: CoordinationCandidates :many
SELECT i.id, i.authorisation_number, i.local_state, i.dss_state, i.time_start, i.time_end, i.volumes,
       i.uspace_airspace_ids, (i.local_state = 'ended')::boolean AS ended
FROM operational_intents i
WHERE i.authorisation_number IS NOT NULL AND NOT i.exempt_art_1_3
  AND NOT EXISTS (SELECT 1 FROM coordination_checks c WHERE c.intent_id = i.id)
  AND (i.local_state IN ('activated', 'nonconforming', 'contingent')
       OR (i.local_state = 'ended'
           AND i.updated_at > now() - make_interval(secs => sqlc.arg(lookback_s)::double precision)
           AND EXISTS (SELECT 1 FROM intent_versions v WHERE v.intent_id = i.id
                       AND v.snapshot -> 'decision' ->> 'state' IN ('activated', 'nonconforming', 'contingent'))))
ORDER BY i.updated_at, i.id
LIMIT sqlc.arg(n);

-- The ended intents judged controlled whose ended notice is not queued.
-- name: CoordinationEnded :many
SELECT i.id, i.authorisation_number, i.local_state, i.dss_state, i.time_start, i.time_end, i.volumes
FROM coordination_checks c
JOIN operational_intents i ON i.id = c.intent_id
WHERE c.controlled AND i.local_state = 'ended'
  AND NOT EXISTS (SELECT 1 FROM coordination_notices n WHERE n.intent_id = i.id AND n.kind = 'ended')
ORDER BY i.updated_at, i.id
LIMIT sqlc.arg(n);

-- name: CoordinationInsertCheck :execrows
INSERT INTO coordination_checks (intent_id, controlled, airspace_ids, unstated_ids, cis_version)
VALUES (sqlc.arg(intent_id)::uuid, sqlc.arg(controlled), sqlc.arg(airspace_ids)::text[], sqlc.arg(unstated_ids)::text[],
        sqlc.narg(cis_version))
ON CONFLICT (intent_id) DO NOTHING;

-- Idempotent by notice_ref: a notice queued before is left as it is
-- (no row). A notice that could not be built is queued failed.
-- name: CoordinationInsertNotice :one
INSERT INTO coordination_notices (notice_ref, kind, intent_id, flight_id, conformance_state_id, payload, body, ack_required,
                                  state, failed_at, last_error)
VALUES (sqlc.arg(notice_ref), sqlc.arg(kind), sqlc.arg(intent_id)::uuid, sqlc.narg(flight_id)::uuid,
        sqlc.narg(conformance_state_id)::bigint, sqlc.arg(payload), sqlc.arg(body), sqlc.arg(ack_required),
        CASE WHEN sqlc.narg(fail_reason)::text IS NULL THEN 'pending' ELSE 'failed' END,
        CASE WHEN sqlc.narg(fail_reason)::text IS NULL THEN NULL ELSE now() END,
        sqlc.narg(fail_reason)::text)
ON CONFLICT (notice_ref) DO NOTHING
RETURNING id;

-- Leases up to n due pending notices, each its intent's oldest pending
-- one, and counts the try.
-- name: CoordinationClaim :many
UPDATE coordination_notices
SET attempts = attempts + 1,
    next_at = now() + make_interval(secs => sqlc.arg(lease_s)::double precision)
WHERE id IN (
    SELECT n.id FROM coordination_notices n
    WHERE n.state = 'pending' AND n.next_at <= now()
      AND NOT EXISTS (SELECT 1 FROM coordination_notices e
                      WHERE e.intent_id = n.intent_id AND e.state = 'pending' AND e.id < n.id)
    ORDER BY n.next_at, n.id
    LIMIT sqlc.arg(n)
    FOR UPDATE SKIP LOCKED
)
RETURNING id, notice_ref, kind, intent_id, conformance_state_id, body, attempts;

-- name: CoordinationReceived :one
UPDATE coordination_notices
SET state = 'received', ack_id = sqlc.arg(ack_id), received_at = now(), ansp_received_at = sqlc.narg(ansp_received_at),
    last_error = NULL,
    poll_next_at = CASE WHEN ack_required AND sqlc.arg(poll_s)::double precision > 0
                        THEN now() + make_interval(secs => sqlc.arg(poll_s)::double precision) END
WHERE id = sqlc.arg(id) AND state = 'pending'
RETURNING conformance_state_id, notice_ref, kind, intent_id;

-- name: ConformanceStateNotified :exec
UPDATE conformance_states SET ats_notified_at = now()
WHERE id = sqlc.arg(id) AND ats_notified_at IS NULL;

-- name: CoordinationRetry :execrows
UPDATE coordination_notices
SET next_at = now() + make_interval(secs => sqlc.arg(backoff_s)::double precision), last_error = sqlc.arg(last_error)
WHERE id = sqlc.arg(id) AND state = 'pending';

-- name: CoordinationFail :one
UPDATE coordination_notices
SET state = 'failed', failed_at = now(), last_error = sqlc.arg(last_error), poll_next_at = NULL
WHERE id = sqlc.arg(id) AND state = 'pending'
RETURNING notice_ref, kind, intent_id;

-- Leases up to n due reads of an acknowledgement for lease_s (another
-- replica skips them meanwhile).
-- name: CoordinationDuePolls :many
UPDATE coordination_notices
SET poll_next_at = now() + make_interval(secs => sqlc.arg(lease_s)::double precision)
WHERE id IN (
    SELECT n.id FROM coordination_notices n
    WHERE n.ack_required AND n.state IN ('received', 'escalated') AND n.poll_next_at <= now()
    ORDER BY n.poll_next_at, n.id
    LIMIT sqlc.arg(n)
    FOR UPDATE SKIP LOCKED
)
RETURNING id, notice_ref, kind, ack_id, conformance_state_id, state, received_at, polls,
          extract(epoch FROM now() - received_at)::double precision AS since_received_s;

-- name: CoordinationPolled :execrows
UPDATE coordination_notices
SET polls = polls + 1,
    poll_next_at = CASE WHEN sqlc.arg(next_s)::double precision > 0
                        THEN now() + make_interval(secs => sqlc.arg(next_s)::double precision) END
WHERE id = sqlc.arg(id) AND state IN ('received', 'escalated');

-- name: CoordinationAcknowledged :one
UPDATE coordination_notices
SET state = 'acknowledged', acknowledged_at = now(), ansp_acknowledged_at = sqlc.narg(ansp_acknowledged_at),
    acknowledged_by = sqlc.narg(acknowledged_by), polls = polls + 1, poll_next_at = NULL
WHERE id = sqlc.arg(id) AND state IN ('received', 'escalated')
RETURNING conformance_state_id, ack_id, notice_ref, kind, intent_id;

-- name: ConformanceStateAcknowledged :exec
UPDATE conformance_states SET ats_ack_ref = sqlc.arg(ats_ack_ref)
WHERE id = sqlc.arg(id) AND ats_ack_ref IS NULL;

-- name: CoordinationEscalate :many
UPDATE coordination_notices
SET state = 'escalated', escalated_at = now()
WHERE ack_required AND state = 'received'
  AND received_at <= now() - make_interval(secs => sqlc.arg(after_s)::double precision)
RETURNING id, notice_ref, kind, intent_id, flight_id, state, created_at, attempts, last_error, ack_id, received_at,
          escalated_at, failed_at, next_at, extract(epoch FROM now() - received_at)::double precision AS age_s;

-- The notices the console must see, oldest first. The age of an
-- escalated notice runs from its receipt, of the others from their
-- queueing.
-- name: CoordinationOpen :many
SELECT id, notice_ref, kind, intent_id, flight_id, state, created_at, attempts, last_error, ack_id, received_at,
       escalated_at, failed_at, next_at,
       extract(epoch FROM now() - coalesce(CASE WHEN state = 'escalated' THEN received_at END, created_at))::double precision AS age_s
FROM coordination_notices
WHERE state IN ('pending', 'escalated', 'failed')
ORDER BY created_at, id
LIMIT sqlc.arg(n);

-- Escalated and failed notices count on /readyz for a day.
-- name: CoordinationSummary :one
SELECT count(*) FILTER (WHERE state = 'pending')::integer AS pending,
       count(*) FILTER (WHERE state = 'pending' AND last_error IS NOT NULL)::integer AS retrying,
       coalesce(extract(epoch FROM now() - min(created_at) FILTER (WHERE state = 'pending')), 0)::double precision AS oldest_pending_s,
       count(*) FILTER (WHERE state = 'escalated' AND created_at > now() - interval '24 hours')::integer AS escalated,
       count(*) FILTER (WHERE state = 'failed' AND created_at > now() - interval '24 hours')::integer AS failed
FROM coordination_notices
WHERE state IN ('pending', 'escalated', 'failed');
