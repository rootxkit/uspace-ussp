-- Service records (internal/records, WP-15; spec 02 F7): the reads of
-- one flight's record, the gap records of telemetry-ingest, the daily
-- bundles. Records carry no names (spec 06 §5): nothing here selects an
-- operator's display name, contact or a free-text field.

-- name: RecordFlight :one
SELECT id, intent_id, authorisation_number, uas_serial, operator_reg, client_id, started_at, ended_at, end_reason,
       rid_flight_id, emergency, last_state
FROM flights
WHERE id = sqlc.arg(id)::uuid;

-- name: RecordIntent :one
SELECT id, local_state, dss_state, priority, decision, authorisation_number, time_start, time_end, volumes,
       deviation_thresholds, conflicts, conditions, cis_version_checked, registry_checked_at, policy_version,
       in_uspace_airspace, uspace_airspace_ids, exempt_art_1_3, mode, flight_type, category, class_label,
       identification_technology, connectivity_methods, endurance_s, operator_reg, ua_registration, uas_serial,
       created_at, updated_at, version
FROM operational_intents
WHERE id = sqlc.arg(id)::uuid;

-- The decision of every version (the request is not copied: its free
-- text may name a person).
-- name: RecordIntentVersions :many
SELECT version, at, actor, change_reason, coalesce(snapshot -> 'decision', 'null'::jsonb)::jsonb AS decision
FROM intent_versions
WHERE intent_id = sqlc.arg(intent_id)::uuid
ORDER BY version
LIMIT sqlc.arg(n);

-- name: RecordAlerts :many
SELECT id, kind, severity, state, raised_at, updated_at, cleared_at, clear_reason, detail, captured_at, policy_version,
       acked_at, escalated_at, peer_ref
FROM alerts
WHERE flight_id = sqlc.arg(flight_id)::uuid
ORDER BY raised_at, id
LIMIT sqlc.arg(n);

-- name: RecordConformance :many
SELECT at, state, reason, distance_outside_m, height_over_m, time_outside_s, policy_version, ats_notified_at, ats_ack_ref
FROM conformance_states
WHERE flight_id = sqlc.arg(flight_id)::uuid
ORDER BY at, id
LIMIT sqlc.arg(n);

-- name: RecordNotices :many
SELECT notice_ref, kind, state, created_at, received_at, ack_id, acknowledged_at, acknowledged_by, escalated_at, failed_at
FROM coordination_notices
WHERE flight_id = sqlc.arg(flight_id)::uuid
   OR (flight_id IS NULL AND intent_id = sqlc.narg(intent_id)::uuid)
ORDER BY created_at, id
LIMIT sqlc.arg(n);

-- name: RecordIngestGaps :many
SELECT cause, gap_started, gap_ended, dropped, from_seq, to_seq, recorded_at
FROM ingest_gaps
WHERE source_instance = sqlc.arg(source_instance)
  AND gap_started IS NOT NULL
  AND gap_ended >= sqlc.arg(from_at) AND gap_started <= sqlc.arg(to_at)
ORDER BY gap_started, id
LIMIT sqlc.arg(n);

-- name: RecordPolicies :many
SELECT version, created_at, "values"
FROM policy
WHERE version = ANY(sqlc.arg(versions)::bigint[])
ORDER BY version;

-- name: InsertIngestGap :execrows
INSERT INTO ingest_gaps (msg_id, source_instance, cause, gap_started, gap_ended, dropped, from_seq, to_seq)
VALUES (sqlc.arg(msg_id), sqlc.arg(source_instance), sqlc.arg(cause), sqlc.narg(gap_started), sqlc.narg(gap_ended),
        sqlc.arg(dropped), sqlc.narg(from_seq), sqlc.narg(to_seq))
ON CONFLICT (msg_id) DO NOTHING;

-- name: PurgeIngestGaps :execrows
DELETE FROM ingest_gaps
WHERE recorded_at < now() - make_interval(days => sqlc.arg(days)::integer);

-- The flights of one UTC day (started in it), in pages after (started_at, id).
-- name: RecordDayFlights :many
SELECT id, started_at
FROM flights
WHERE started_at >= sqlc.arg(day_start) AND started_at < sqlc.arg(day_end)
  AND (started_at, id) > (sqlc.arg(after_at)::timestamptz, sqlc.arg(after_id)::uuid)
ORDER BY started_at, id
LIMIT sqlc.arg(n);

-- name: InsertRecordBundle :execrows
INSERT INTO record_bundles (date, built_at, content_hash, storage_ref, flights)
VALUES (sqlc.arg(date), now(), sqlc.arg(content_hash), sqlc.arg(storage_ref), sqlc.arg(flights))
ON CONFLICT (date) DO NOTHING;

-- name: GetRecordBundle :one
SELECT date, built_at, content_hash, storage_ref, flights
FROM record_bundles
WHERE date = sqlc.arg(date);

-- The days from first to last (inclusive) without a bundle.
-- name: MissingRecordDays :many
SELECT d::date AS day
FROM generate_series(sqlc.arg(first_day)::date, sqlc.arg(last_day)::date, interval '1 day') AS d
WHERE NOT EXISTS (SELECT 1 FROM record_bundles b WHERE b.date = d::date)
ORDER BY d;
