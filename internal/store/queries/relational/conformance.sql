-- Conformance (internal/conformance, WP-10): the timeline of a flight's
-- conformance states, written by api from conf.v1 (docs/PLAN.md §3.2).
-- Append-only (the application role has INSERT and SELECT only). A row
-- is written only for a flight the flights table holds: 0 rows means the
-- flight fact has not been recorded yet and the consumer tries again.

-- The last position is the one the monitor reported with the state
-- (WP-15: the nonconformance notice to the ANSP carries it).
-- name: InsertConformanceState :execrows
INSERT INTO conformance_states (flight_id, at, state, reason, distance_outside_m, height_over_m, time_outside_s, policy_version,
                                last_lat_deg, last_lng_deg)
SELECT f.id, sqlc.arg(at), sqlc.arg(state), sqlc.narg(reason),
       sqlc.narg(distance_outside_m)::double precision, sqlc.narg(height_over_m)::double precision,
       sqlc.narg(time_outside_s)::double precision, sqlc.arg(policy_version),
       sqlc.narg(last_lat_deg)::double precision, sqlc.narg(last_lng_deg)::double precision
FROM flights f
WHERE f.id = sqlc.arg(flight_id)::uuid;

-- name: ConformanceTimeline :many
SELECT at, state, reason, distance_outside_m, height_over_m, time_outside_s, policy_version
FROM conformance_states
WHERE flight_id = sqlc.arg(flight_id)::uuid
ORDER BY at, id
LIMIT sqlc.arg(max_rows);
