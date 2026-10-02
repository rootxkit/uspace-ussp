-- The CIS cache of WP-4 (internal/cis): dataset versions and their
-- features, the change-notification log and its replay guard.

-- name: InsertCISDataset :one
-- A version is stored once; a second store of the same version is a
-- no-op (inserted = false). fetched_at is the database clock.
WITH ins AS (
    INSERT INTO cis_datasets (dataset, version, etag, fetched_at, metadata, signature_ok)
    VALUES (sqlc.arg(dataset), sqlc.arg(version), sqlc.narg(etag), now(), sqlc.arg(metadata), sqlc.arg(signature_ok))
    ON CONFLICT (dataset, version) DO NOTHING
    RETURNING fetched_at
)
SELECT (SELECT count(*) FROM ins) = 1 AS inserted,
       COALESCE((SELECT fetched_at FROM ins), now())::timestamptz AS fetched_at;

-- name: InsertCISFeature :exec
-- geom is the union of the feature's parts as PostGIS builds them: each
-- polygon from its GeoJSON (polygons: a JSON array of GeoJSON
-- Polygons), each circle as the geography buffer of its published
-- centre and radius in metres (circles: a JSON array of {lon_deg,
-- lat_deg, radius_m}) (a stored shape for queries,
-- never the judgement, which is uspace-core zones on the published
-- circle, LESSONS Z-11).
INSERT INTO cis_features (dataset, version, feature_id, feature, geom,
                          lower_m, lower_ref, upper_m, upper_ref,
                          applicable_from, applicable_to, zone_type)
SELECT sqlc.arg(dataset), sqlc.arg(version), sqlc.arg(feature_id), sqlc.arg(feature),
       (SELECT ST_Collect(part)::geography FROM (
            SELECT ST_SetSRID(ST_GeomFromGeoJSON(p.value::text), 4326) AS part
              FROM jsonb_array_elements(sqlc.arg(polygons)::jsonb) AS p
            UNION ALL
            SELECT ST_Buffer(ST_SetSRID(ST_MakePoint((c.value->>'lon_deg')::float8, (c.value->>'lat_deg')::float8), 4326)::geography,
                             (c.value->>'radius_m')::float8)::geometry
              FROM jsonb_array_elements(sqlc.arg(circles)::jsonb) AS c
        ) AS parts),
       sqlc.narg(lower_m), sqlc.narg(lower_ref), sqlc.narg(upper_m), sqlc.narg(upper_ref),
       sqlc.narg(applicable_from), sqlc.narg(applicable_to), sqlc.narg(zone_type);

-- name: PruneCISVersions :execrows
-- Keeps the current and the previous version of a dataset (history is
-- the CISP's), and the newest one whose publisher signature verified
-- (signature_ok): versions held untrusted never push out the one in
-- use. The features go with their version (ON DELETE CASCADE).
DELETE FROM cis_datasets d
 WHERE d.dataset = sqlc.arg(dataset)
   AND d.version < (SELECT min(k.version) FROM (
           SELECT version FROM cis_datasets WHERE dataset = sqlc.arg(dataset) ORDER BY version DESC LIMIT 2) AS k)
   AND d.version IS DISTINCT FROM (SELECT max(t.version) FROM cis_datasets t
                                    WHERE t.dataset = sqlc.arg(dataset) AND t.signature_ok);

-- name: TouchCISDataset :one
-- The CISP confirmed this version is current (a 304, or the same
-- version again): fetched_at moves to the database clock.
UPDATE cis_datasets SET fetched_at = now()
 WHERE dataset = sqlc.arg(dataset) AND version = sqlc.arg(version)
RETURNING fetched_at;

-- name: CurrentCISDatasets :many
-- The newest stored version of every dataset whose publisher signature
-- verified (a held one is never loaded), with its age on the database
-- clock.
SELECT DISTINCT ON (dataset) dataset, version, etag, fetched_at, metadata, signature_ok,
       extract(epoch FROM now() - fetched_at)::float8 AS age_s
  FROM cis_datasets
 WHERE signature_ok
 ORDER BY dataset, version DESC;

-- name: CISFeatures :many
SELECT feature_id, feature FROM cis_features
 WHERE dataset = sqlc.arg(dataset) AND version = sqlc.arg(version)
 ORDER BY feature_id;

-- name: RememberCISJTI :one
-- Records a verified delivery id. inserted is false for a delivery
-- already seen (a replay) and when the live rows have reached
-- max_rows (live says which). expires_at is the database clock plus
-- ttl_s.
WITH live AS (
    SELECT count(*) AS n FROM cis_notification_jtis WHERE expires_at > now()
), ins AS (
    INSERT INTO cis_notification_jtis (issuer, jti, expires_at)
    SELECT sqlc.arg(issuer), sqlc.arg(jti), now() + make_interval(secs => sqlc.arg(ttl_s)::float8)
      FROM live WHERE live.n < sqlc.arg(max_rows)::bigint
    ON CONFLICT (issuer, jti) DO NOTHING
    RETURNING 1
)
SELECT (SELECT count(*) FROM ins) = 1 AS inserted, (SELECT n FROM live)::bigint AS live;

-- name: SweepCISJTIs :execrows
DELETE FROM cis_notification_jtis WHERE expires_at <= now();

-- name: InsertCISNotification :one
INSERT INTO cis_notifications (dataset, version, feature_ids, reason, jws_ok, issuer, jti, subscription, msg_id)
VALUES (sqlc.arg(dataset), sqlc.narg(version), sqlc.arg(feature_ids), sqlc.arg(reason), true,
        sqlc.arg(issuer), sqlc.arg(jti), sqlc.arg(subscription), sqlc.arg(msg_id))
RETURNING id;

-- name: MarkCISNotificationsPulled :execrows
-- Every notification of the dataset up to the version now held.
UPDATE cis_notifications SET pulled_at = now()
 WHERE dataset = sqlc.arg(dataset) AND pulled_at IS NULL
   AND (version IS NULL OR version <= sqlc.arg(version));

-- name: SweepCISNotifications :execrows
DELETE FROM cis_notifications
 WHERE received_at < now() - make_interval(days => sqlc.arg(keep_days)::int);
