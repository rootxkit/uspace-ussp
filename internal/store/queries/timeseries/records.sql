-- Service records (internal/records, WP-15): what api reads of the
-- hypertables for one flight's record (read only; tsdb-writer writes).

-- The telemetry summary of a flight: samples, first and last capture,
-- the box of its positions (WGS84 degrees) and the highest AMSL altitude
-- (has_alt_amsl false when no sample had one: not judged, never 0). The
-- other columns mean nothing when samples is 0.
-- name: RecordTelemetrySummary :one
SELECT count(*)::bigint AS samples,
       coalesce(min(captured_at), 'epoch'::timestamptz)::timestamptz AS first_at,
       coalesce(max(captured_at), 'epoch'::timestamptz)::timestamptz AS last_at,
       coalesce(min(ST_X(geom)), 0)::double precision AS min_lng,
       coalesce(min(ST_Y(geom)), 0)::double precision AS min_lat,
       coalesce(max(ST_X(geom)), 0)::double precision AS max_lng,
       coalesce(max(ST_Y(geom)), 0)::double precision AS max_lat,
       coalesce(bool_or(alt_amsl_m IS NOT NULL), false)::boolean AS has_alt_amsl,
       coalesce(max(alt_amsl_m), 0)::double precision AS max_alt_amsl_m
FROM telemetry
WHERE flight_id = sqlc.arg(flight_id)::uuid;

-- Every silence longer than gap_s between two samples of a flight
-- (B-13: the track is cut there), oldest first.
-- name: RecordTelemetrySilences :many
SELECT prev_at::timestamptz AS after_at, captured_at::timestamptz AS before_at
FROM (
    SELECT captured_at, lag(captured_at) OVER (ORDER BY captured_at) AS prev_at
    FROM telemetry
    WHERE flight_id = sqlc.arg(flight_id)::uuid
) s
WHERE prev_at IS NOT NULL AND captured_at - prev_at > make_interval(secs => sqlc.arg(gap_s)::double precision)
ORDER BY captured_at
LIMIT sqlc.arg(n);

-- The holes tsdb-writer recorded in the TRK stream (own flights'
-- telemetry) around a window.
-- name: RecordWriterGaps :many
SELECT cause, count, count_unit, after_at, before_at, detail, at
FROM writer_gaps
WHERE stream = 'TRK'
  AND coalesce(after_at, at) <= sqlc.arg(to_at) AND coalesce(before_at, at) >= sqlc.arg(from_at)
ORDER BY at
LIMIT sqlc.arg(n);

-- What a client was shown in a window (the 0.1 Hz sample of traffic-ws).
-- name: RecordTrafficProducts :many
SELECT at, intent_id, tracks_shown, degraded, policy_version
FROM traffic_products
WHERE client_id = sqlc.arg(client_id) AND at >= sqlc.arg(from_at) AND at <= sqlc.arg(to_at)
ORDER BY at
LIMIT sqlc.arg(n);

-- name: CountTrafficProducts :one
SELECT count(*)::bigint
FROM traffic_products
WHERE client_id = sqlc.arg(client_id) AND at >= sqlc.arg(from_at) AND at <= sqlc.arg(to_at);

-- The newest capture time of each of the flights (the console's flights
-- page, WP-18), within the last day; a flight without a sample in it has
-- no row.
-- name: ConsoleLastSamples :many
SELECT flight_id, max(captured_at)::timestamptz AS last_at
FROM telemetry
WHERE flight_id = ANY (sqlc.arg(flight_ids)::uuid[]) AND captured_at > now() - interval '1 day'
GROUP BY flight_id;
