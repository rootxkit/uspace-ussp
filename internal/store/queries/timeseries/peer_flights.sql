-- peer_flights (tsdb-writer writes; the retention policy removes rows
-- after 24 h).

-- name: InsertPeerFlight :exec
INSERT INTO peer_flights (peer_uss, rid_flight_id, rx_ts, state, details, isa_id, cell5)
VALUES (sqlc.arg(peer_uss), sqlc.arg(rid_flight_id), sqlc.arg(rx_ts), sqlc.arg(state), sqlc.narg(details), sqlc.narg(isa_id), sqlc.narg(cell5));

-- name: CountPeerFlights :one
SELECT count(*) FROM peer_flights
WHERE peer_uss = sqlc.arg(peer_uss) AND rid_flight_id = sqlc.arg(rid_flight_id);
