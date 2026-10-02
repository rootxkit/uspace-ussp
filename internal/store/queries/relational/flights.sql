-- Flights (internal/flights): the facts telemetry-ingest publishes on
-- flight.v1, recorded by api (docs/PLAN.md §3.2). Every event carries the
-- whole flight, so any one of them creates the row; a later one never
-- reopens an ended flight. A client or an intent this database does not
-- hold is recorded as NULL, never refused: the flight happened.

-- name: RecordFlightEvent :exec
INSERT INTO flights (id, intent_id, authorisation_number, uas_serial, operator_reg, client_id, started_at, ended_at, end_reason, last_state)
VALUES (
    sqlc.arg(id)::uuid,
    (SELECT oi.id FROM operational_intents oi WHERE oi.id = sqlc.narg(intent_id)::uuid),
    sqlc.narg(authorisation_number),
    sqlc.arg(uas_serial),
    sqlc.narg(operator_reg),
    (SELECT c.client_id FROM oauth_clients c WHERE c.client_id = sqlc.arg(client_id)::text),
    sqlc.arg(started_at),
    CASE WHEN sqlc.narg(ended_at)::timestamptz IS NULL THEN NULL
         ELSE GREATEST(sqlc.narg(ended_at)::timestamptz, sqlc.arg(started_at)::timestamptz) END,
    sqlc.narg(end_reason),
    sqlc.arg(last_state)
)
ON CONFLICT (id) DO UPDATE
SET ended_at = COALESCE(flights.ended_at, EXCLUDED.ended_at),
    end_reason = COALESCE(flights.end_reason, EXCLUDED.end_reason),
    last_state = CASE WHEN flights.ended_at IS NULL THEN EXCLUDED.last_state ELSE flights.last_state END;

-- name: GetFlight :one
SELECT id, intent_id, authorisation_number, uas_serial, operator_reg, client_id, started_at, ended_at, end_reason, last_state
FROM flights
WHERE id = sqlc.arg(id)::uuid;
