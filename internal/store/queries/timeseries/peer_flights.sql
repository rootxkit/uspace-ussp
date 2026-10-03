-- peer_flights (tsdb-writer writes; the retention policy removes rows
-- after 24 h).

-- name: InsertPeerFlight :exec
INSERT INTO peer_flights (peer_uss, rid_flight_id, rx_ts, state, details, isa_id, cell5)
VALUES (sqlc.arg(peer_uss), sqlc.arg(rid_flight_id), sqlc.arg(rx_ts), sqlc.arg(state), sqlc.narg(details), sqlc.narg(isa_id), sqlc.narg(cell5));

-- name: CountPeerFlights :one
SELECT count(*) FROM peer_flights
WHERE peer_uss = sqlc.arg(peer_uss) AND rid_flight_id = sqlc.arg(rid_flight_id);

-- The newest sample of a flight captured after since (GET
-- /uss/v1/operational_intents/{id}/telemetry, internal/dss): position,
-- WGS84 altitude, speed and track as the operator's telemetry gave them.
-- name: LatestFlightSample :one
SELECT captured_at, ST_Y(geom)::double precision AS lat, ST_X(geom)::double precision AS lng,
       alt_wgs84_m, speed_ms, track_deg, accuracy_h, accuracy_v
FROM telemetry
WHERE flight_id = sqlc.arg(flight_id)::uuid AND captured_at > sqlc.arg(since)::timestamptz
ORDER BY captured_at DESC
LIMIT 1;
