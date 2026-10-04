-- Weather products and the state of their source (internal/weather,
-- WP-16; migrations 00005, 00024). Every time is the database clock.

-- name: WeatherNow :one
SELECT now()::timestamptz AS now;

-- A report already held is left as it is (every api instance polls).
-- name: WeatherInsert :execrows
INSERT INTO weather_products (area, observed_at, valid_from, valid_to, source, product, fetched_at, station, kind)
VALUES (
    ST_Buffer(ST_SetSRID(ST_MakePoint(sqlc.arg(lon_deg)::double precision, sqlc.arg(lat_deg)::double precision), 4326)::geography,
              sqlc.arg(radius_m)::double precision),
    sqlc.arg(observed_at), sqlc.arg(valid_from), sqlc.arg(valid_to), sqlc.arg(source), sqlc.arg(product), now(),
    sqlc.arg(station), sqlc.arg(kind))
ON CONFLICT (source, station, kind, observed_at) DO NOTHING;

-- A success clears last_error (the source is no longer failing) and
-- keeps last_failure_at.
-- name: WeatherFetchSucceeded :exec
INSERT INTO weather_source_status (source, last_attempt_at, last_success_at)
VALUES (sqlc.arg(source), now(), now())
ON CONFLICT (source) DO UPDATE SET last_attempt_at = now(), last_success_at = now(), last_error = NULL;

-- name: WeatherFetchFailed :exec
INSERT INTO weather_source_status (source, last_attempt_at, last_failure_at, last_error)
VALUES (sqlc.arg(source), now(), now(), sqlc.arg(last_error))
ON CONFLICT (source) DO UPDATE SET last_attempt_at = now(), last_failure_at = now(), last_error = sqlc.arg(last_error);

-- name: WeatherSourceStatus :one
SELECT * FROM weather_source_status WHERE source = sqlc.arg(source);

-- The newest product of each station and kind whose area meets the box
-- and that was issued at or before at.
-- name: WeatherNewest :many
SELECT DISTINCT ON (station, kind) id, station, kind, source, observed_at, valid_from, valid_to, fetched_at, product
FROM weather_products
WHERE source = sqlc.arg(source)
  AND observed_at <= sqlc.arg(at_time)::timestamptz
  AND ST_Intersects(area, ST_MakeEnvelope(sqlc.arg(west)::double precision, sqlc.arg(south)::double precision,
                                          sqlc.arg(east)::double precision, sqlc.arg(north)::double precision, 4326)::geography)
ORDER BY station, kind, observed_at DESC
LIMIT sqlc.arg(n);

-- The products whose area meets one of the boxes (as WKT polygons) and
-- whose validity overlaps [from, to], newest first.
-- name: WeatherInForce :many
SELECT id, station, kind, source, observed_at, valid_from, valid_to, fetched_at, product
FROM weather_products
WHERE source = sqlc.arg(source)
  AND valid_from <= sqlc.arg(to_time)::timestamptz
  AND valid_to >= sqlc.arg(from_time)::timestamptz
  AND ST_Intersects(area, ST_GeogFromText(sqlc.arg(boxes_wkt)::text))
ORDER BY observed_at DESC, id
LIMIT sqlc.arg(n);

-- name: WeatherPrune :execrows
DELETE FROM weather_products
WHERE id IN (SELECT id FROM weather_products WHERE valid_to < sqlc.arg(before)::timestamptz ORDER BY valid_to LIMIT sqlc.arg(n));
