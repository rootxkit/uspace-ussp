-- Occurrence reports to the authority (internal/occurrence, WP-15).
-- Every time is the database clock.

-- The alerts raised within lookback that are occurrences and are not
-- reported yet: a proximity alert whose closest approach is within the
-- airprox thresholds (an unjudged vertical counts as within), a zone
-- incursion on a PROHIBITED zone, a lost link of an intent in U-space
-- airspace. source_ref keys the report: a proximity conflict by its pair
-- and raise (both flights' alerts are one report), any other by the
-- alert id.
-- name: OccurrenceAlertEvents :many
SELECT a.id, a.kind, a.raised_at, a.detail, src.ref AS source_ref,
       f.id AS flight_id, f.uas_serial, f.operator_reg, f.authorisation_number, f.intent_id, f.emergency, f.started_at,
       coalesce(i.in_uspace_airspace, false)::boolean AS in_uspace
FROM alerts a
JOIN flights f ON f.id = a.flight_id
LEFT JOIN operational_intents i ON i.id = f.intent_id
CROSS JOIN LATERAL (
    SELECT CASE WHEN a.kind = 'proximity' AND a.detail ? 'pair_id'
                THEN 'pair:' || (a.detail ->> 'pair_id') || '@' || floor(extract(epoch FROM a.raised_at) * 1000)::bigint::text
                ELSE a.id::text END AS ref
) src
WHERE a.raised_at > now() - make_interval(secs => sqlc.arg(lookback_s)::double precision)
  AND a.kind IN ('proximity', 'zone_incursion', 'lost_link')
  AND (
        (a.kind = 'proximity'
         AND jsonb_typeof(a.detail -> 'd_cpa_h_m') = 'number'
         AND (a.detail ->> 'd_cpa_h_m')::double precision < sqlc.arg(airprox_h_m)::double precision
         AND (jsonb_typeof(a.detail -> 'd_alt_m') IS DISTINCT FROM 'number'
              OR abs((a.detail ->> 'd_alt_m')::double precision) < sqlc.arg(airprox_v_m)::double precision))
     OR (a.kind = 'zone_incursion' AND a.detail ->> 'zone_type' = 'PROHIBITED')
     OR (a.kind = 'lost_link' AND coalesce(i.in_uspace_airspace, false))
  )
  AND NOT EXISTS (SELECT 1 FROM occurrence_reports r WHERE r.source_kind = 'alert' AND r.source_ref = src.ref)
ORDER BY a.raised_at, a.id
LIMIT sqlc.arg(n);

-- name: OccurrenceEmergencyFlights :many
SELECT f.id AS flight_id, f.uas_serial, f.operator_reg, f.authorisation_number, f.intent_id, f.emergency, f.started_at,
       coalesce(i.in_uspace_airspace, false)::boolean AS in_uspace
FROM flights f
LEFT JOIN operational_intents i ON i.id = f.intent_id
WHERE f.emergency
  AND f.started_at > now() - make_interval(secs => sqlc.arg(lookback_s)::double precision)
  AND NOT EXISTS (SELECT 1 FROM occurrence_reports r WHERE r.source_kind = 'flight' AND r.source_ref = f.id::text)
ORDER BY f.started_at, f.id
LIMIT sqlc.arg(n);

-- name: OccurrenceAlert :one
SELECT a.id, a.kind, a.raised_at, a.detail,
       CASE WHEN a.kind = 'proximity' AND a.detail ? 'pair_id'
            THEN 'pair:' || (a.detail ->> 'pair_id') || '@' || floor(extract(epoch FROM a.raised_at) * 1000)::bigint::text
            ELSE a.id::text END::text AS source_ref,
       f.id AS flight_id, f.uas_serial, f.operator_reg, f.authorisation_number, f.intent_id, f.emergency, f.started_at,
       coalesce(i.in_uspace_airspace, false)::boolean AS in_uspace
FROM alerts a
JOIN flights f ON f.id = a.flight_id
LEFT JOIN operational_intents i ON i.id = f.intent_id
WHERE a.id = sqlc.arg(id)::uuid;

-- name: OccurrenceFlights :many
SELECT f.id AS flight_id, f.uas_serial, f.operator_reg, f.authorisation_number, f.intent_id, f.emergency, f.started_at,
       coalesce(i.in_uspace_airspace, false)::boolean AS in_uspace
FROM flights f
LEFT JOIN operational_intents i ON i.id = f.intent_id
WHERE f.id = ANY(sqlc.arg(ids)::uuid[]);

-- Idempotent by source: a report of an event reported before is left as
-- it is (no row).
-- name: OccurrenceInsert :one
INSERT INTO occurrence_reports (report_ref, kind, occurred_at, became_aware_at, deadline_at, payload, source_kind, source_ref,
                                channel, flagged_by, reporter_ref, flight_ids, intent_ids)
VALUES (sqlc.arg(report_ref), sqlc.arg(kind), sqlc.arg(occurred_at), sqlc.arg(became_aware_at),
        sqlc.arg(became_aware_at)::timestamptz + make_interval(secs => sqlc.arg(deadline_s)::double precision),
        sqlc.arg(payload), sqlc.arg(source_kind), sqlc.arg(source_ref), sqlc.arg(channel), sqlc.arg(flagged_by),
        sqlc.arg(reporter_ref), sqlc.arg(flight_ids)::uuid[], sqlc.arg(intent_ids)::uuid[])
ON CONFLICT (source_kind, source_ref) DO NOTHING
RETURNING id;

-- name: OccurrenceBySource :one
SELECT id, report_ref, kind, state, channel, flagged_by, became_aware_at, deadline_at, attempts, last_error, flight_ids,
       submitted_at, authority_ref, failed_at, next_at,
       extract(epoch FROM deadline_at - now())::double precision AS time_to_deadline_s
FROM occurrence_reports
WHERE source_kind = sqlc.arg(source_kind) AND source_ref = sqlc.arg(source_ref);

-- name: OccurrenceClaim :many
UPDATE occurrence_reports
SET attempts = attempts + 1, next_at = now() + make_interval(secs => sqlc.arg(lease_s)::double precision)
WHERE id IN (
    SELECT r.id FROM occurrence_reports r
    WHERE r.state = 'pending' AND r.next_at <= now()
    ORDER BY r.deadline_at, r.id
    LIMIT sqlc.arg(n)
    FOR UPDATE SKIP LOCKED
)
RETURNING id, report_ref, payload, attempts;

-- name: OccurrenceDelivered :one
UPDATE occurrence_reports
SET state = 'delivered', submitted_at = now(), authority_ref = sqlc.arg(authority_ref), last_error = NULL
WHERE id = sqlc.arg(id)::uuid AND state = 'pending'
RETURNING report_ref, kind;

-- name: OccurrenceRetry :execrows
UPDATE occurrence_reports
SET next_at = now() + make_interval(secs => sqlc.arg(backoff_s)::double precision), last_error = sqlc.arg(last_error)
WHERE id = sqlc.arg(id)::uuid AND state = 'pending';

-- name: OccurrenceFail :one
UPDATE occurrence_reports
SET state = 'failed', failed_at = now(), last_error = sqlc.arg(last_error)
WHERE id = sqlc.arg(id)::uuid AND state = 'pending'
RETURNING report_ref, kind;

-- The reports not delivered, the nearest deadline first.
-- name: OccurrenceOpen :many
SELECT id, report_ref, kind, state, channel, flagged_by, became_aware_at, deadline_at, attempts, last_error, flight_ids,
       submitted_at, authority_ref, failed_at, next_at,
       extract(epoch FROM deadline_at - now())::double precision AS time_to_deadline_s
FROM occurrence_reports
WHERE state <> 'delivered'
ORDER BY deadline_at, id
LIMIT sqlc.arg(n);

-- name: OccurrenceSummary :one
SELECT count(*) FILTER (WHERE state = 'pending')::integer AS pending,
       count(*) FILTER (WHERE state = 'failed')::integer AS failed,
       count(*) FILTER (WHERE deadline_at < now())::integer AS critical,
       coalesce(extract(epoch FROM min(deadline_at) - now()), 0)::double precision AS nearest_deadline_s
FROM occurrence_reports
WHERE state <> 'delivered';

-- name: OccurrenceHeld :many
SELECT DISTINCT unnest(flight_ids)::text AS flight_id
FROM occurrence_reports;
