#!/usr/bin/env bash
# shellcheck disable=SC2016 # SQL, jq and the containers' shells name their own $variables
# The staging smoke (WP-20; `make staging-smoke`, CI job `staging-smoke`):
# deploy/compose/staging.yaml brought up from scratch the way
# deploy/deploy.sh deploys it, behind the edge Caddy with
# deploy/caddy/ussp.caddy, then used through the public host by a
# simulated operator, restarted, rolled one migration back and forward,
# and torn down. Every step says what it saw; any step that fails stops
# the run. Nothing here reaches a server: every container is local and
# every peer is a fake.
#
#   SMOKE_GO_IMAGE=uspace-ussp:smoke SMOKE_WEB_IMAGE=uspace-ussp-web:smoke deploy/smoke/run.sh
#
# Steps:
#    1. secrets (deploy/gen-secrets.sh), the certificate files in the form
#       uspace-deploy's scripts/ussp-certificate.sh writes them, a
#       throwaway root CA, a flat geoid grid, the edge network with the
#       addresses staging.yaml trusts;
#    2. the peers (TestStagingSmokePeers: the authority, the CISP, the
#       ANSP and the DSS as fakes) and the settings pointing at them; the
#       edge Caddy;
#    3. deploy/deploy.sh --unsigned-local-images --strict: preflight,
#       migrate (both trees), the rolling start, the verification, with
#       only the dependencies the smoke cannot have accepted by name;
#    4. the same verification without those acceptances must fail and
#       name them (the deploy's failure path);
#    5. the simulated operator through the public host (TestStagingSmoke):
#       registration, sign-in, client, serial, token, an intent authorised
#       and activated, telemetry acknowledged; it must pass, not skip;
#    6. a backup (deploy/backup.sh) with the writers paused, a telemetry
#       row older than the time-series window among the rows, and two of a
#       held flight (one an occurrence report names): the restore
#       check must give back every count but the unheld row, with the held
#       rows and every record bundle, and must refuse
#       counts that differ and a file that differs from its manifest;
#    7. the whole stack stopped and removed (volumes kept), deployed again,
#       and the operator's intent read back after it;
#    8. one migration of each tree rolled back: the process that needs it
#       refuses to start and names both versions; migrate brings it back;
#    9. memory and CPU of every container (docker stats);
#   10. teardown (also on failure): the project's containers, volumes and
#       networks removed and their absence checked; the peers stopped.
#
# SMOKE_VERSION, when set, is the VERSION the images were built with;
# every process must report it. SMOKE_LOG_DIR, when set, receives every
# container's log and the peers' on failure (the CI artifact).
set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
deploy="$(cd "$here/.." && pwd)"
repo="$(cd "$deploy/.." && pwd)"
project="${SMOKE_PROJECT:-uspace-ussp-smoke}"
edge="${project}-edge"
subnet="${SMOKE_EDGE_SUBNET:-172.31.251.0/24}"
prefix="${subnet%.*}"
port="${SMOKE_HTTPS_PORT:-58443}"
peers_port="${SMOKE_PEERS_PORT:-58610}"
control="127.0.0.1:${SMOKE_CONTROL_PORT:-58619}"
host=ussp.smoke.test
code=SMK01
go_image="${SMOKE_GO_IMAGE:?the Go image to deploy (a local tag)}"
web_image="${SMOKE_WEB_IMAGE:?the web image to deploy (a local tag)}"
GO="${GO:-go}"

work="$(mktemp -d)"
if type -P cygpath >/dev/null 2>&1; then
  work="$(cygpath -m "$work")"
  repo="$(cygpath -m "$repo")"
  deploy="$(cygpath -m "$deploy")"
  here="$(cygpath -m "$here")"
fi
step() { echo; echo "smoke: ---- $*"; }
fail() { echo "smoke: FAIL $*" >&2; exit 1; }
peers_pid=""

dc() {
  docker compose -p "$project" --env-file "$work/deploy.env" --env-file "$work/settings.env" \
    -f "$deploy/compose/staging.yaml" -f "$here/compose.smoke.yaml" "$@"
}

# shellcheck disable=SC2317 # run by the EXIT trap
teardown() {
  local rc=$?
  echo
  echo "smoke: ---- 10. teardown"
  if [ "$rc" -ne 0 ] && [ -n "${SMOKE_LOG_DIR:-}" ]; then
    mkdir -p "$SMOKE_LOG_DIR"
    if [ -f "$work/deploy.env" ] && [ -f "$work/settings.env" ]; then
      for s in $(dc config --services 2>/dev/null); do
        dc logs --no-log-prefix "$s" > "$SMOKE_LOG_DIR/$s.log" 2>&1 || true
      done
    fi
    cp "$work/peers.log" "$SMOKE_LOG_DIR/peers.log" 2>/dev/null || true
    echo "smoke: logs written to $SMOKE_LOG_DIR"
  fi
  if [ -n "$peers_pid" ]; then
    curl -s --max-time 5 -X POST "http://$control/stop" >/dev/null 2>&1 || true
    # Bounded: the peers stop within their 5 s shutdown, or are killed.
    if ! timeout 20 tail --pid="$peers_pid" -f /dev/null 2>/dev/null; then kill "$peers_pid" 2>/dev/null || true; fi
    if kill -0 "$peers_pid" 2>/dev/null; then
      echo "smoke: FAIL the peers (pid $peers_pid) are still running" >&2
      rc=1
    else
      echo "smoke: peers stopped"
    fi
  fi
  if ! "$deploy/teardown.sh" --project "$project" --volumes --network "$edge"; then rc=1; fi
  rm -rf "$work"
  exit "$rc"
}
trap teardown EXIT

# ---- 1 -----------------------------------------------------------------------
step "1. secrets, certificate, CA, geoid, edge network"
"$deploy/gen-secrets.sh" "$work/secrets" | tail -n 1
# The Go containers run as the image's user (65532), which must read the
# keys of this throwaway directory; on a server gen-secrets --owner gives
# them to it instead.
chmod 0755 "$work/secrets/keys"
# As scripts/ussp-certificate.sh writes them from the authority's answer
# (uspace-deploy): the id, and the client secret without a newline.
cert_id="$(openssl rand -hex 16)"
printf 'USSP_CERTIFICATE_ID=%s\n' "$cert_id" > "$work/secrets/certificate.env"
openssl rand -hex 24 | tr -d '\n' > "$work/secrets/keys/client.secret"
chmod 0644 "$work/secrets/keys/"*
mkdir -p "$work/ca" "$work/basemap" "$work/ground" "$work/state" "$work/backups"
MSYS_NO_PATHCONV=1 openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 2 \
  -subj "/O=uspace-ussp smoke/CN=smoke root" \
  -addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign,cRLSign" \
  -keyout "$work/ca/ca.key" -out "$work/ca/ca.pem" >/dev/null 2>&1
chmod 0644 "$work/ca/ca.key" "$work/ca/ca.pem"
# The peers' certificate for the name the containers use (the processes
# fetch every JWKS over https only).
MSYS_NO_PATHCONV=1 openssl req -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -subj "/CN=smoke-peers" \
  -keyout "$work/ca/peers.key" -out "$work/ca/peers.csr" >/dev/null 2>&1
printf 'subjectAltName=DNS:smoke-peers\nextendedKeyUsage=serverAuth\n' > "$work/ca/peers.ext"
MSYS_NO_PATHCONV=1 openssl x509 -req -in "$work/ca/peers.csr" -CA "$work/ca/ca.pem" -CAkey "$work/ca/ca.key" \
  -CAcreateserial -days 2 -extfile "$work/ca/peers.ext" -out "$work/ca/peers.pem" >/dev/null 2>&1
printf '{"release":"smoke"}\n' > "$work/basemap/SOURCE.json"
# A flat 20 m geoid (test/e2e/stack's), named as the real grid is: the
# smoke checks the deployment, not the geoid.
{ printf 'P5\n# Description smoke grid, N = 20 m\n# Offset 20\n# Scale 1\n2 3\n65535\n'; head -c 12 /dev/zero; } > "$work/ground/egm2008-2_5.pgm"
chmod -R a+rX "$work/ground" "$work/basemap"
docker network create --subnet "$subnet" --gateway "$prefix.1" --ip-range "$prefix.128/25" "$edge" >/dev/null
# Where the containers reach the peers on the host: on Linux the edge
# bridge's own address (the host's interface on it, nothing else hears
# it); under Docker Desktop the host's loopback through host-gateway.
if docker info --format '{{.OperatingSystem}}' 2>/dev/null | grep -qi 'docker desktop'; then
  peers_bind=127.0.0.1 peers_ip=host-gateway
else
  # Set at creation: a newer Docker leaves IPAM.Config.Gateway empty
  # unless it was given.
  peers_bind="$(docker network inspect "$edge" --format '{{(index .IPAM.Config 0).Gateway}}')"
  peers_ip="$peers_bind"
fi
[ -n "$peers_bind" ] || fail "the edge network has no gateway address"
echo "smoke: edge network $edge ($subnet; Caddy $prefix.2, web $prefix.3); peers on $peers_bind, certificate $cert_id"

# ---- 2 -----------------------------------------------------------------------
step "2. peers, settings, Caddy"
exe=""; if type -P cygpath >/dev/null 2>&1; then exe=.exe; fi
smoke_test="$work/smoke.test$exe"
(cd "$repo" && "$GO" test -c -o "$smoke_test" ./deploy/smoke)
STAGING_SMOKE_PEERS=1 STAGING_SMOKE_PEERS_BIND="$peers_bind" STAGING_SMOKE_PEERS_HOST=smoke-peers \
  STAGING_SMOKE_PEERS_CERT="$work/ca/peers.pem" STAGING_SMOKE_PEERS_KEY="$work/ca/peers.key" \
  STAGING_SMOKE_PEERS_PORT="$peers_port" STAGING_SMOKE_CONTROL="$control" STAGING_SMOKE_PEERS_ENV="$work/peers.env" \
  "$smoke_test" -test.run '^TestStagingSmokePeers$' -test.v -test.timeout 45m > "$work/peers.log" 2>&1 &
peers_pid=$!
# The condition, by curl's bounded retries: at most 60 tries a second apart.
curl -s --max-time 2 --retry 60 --retry-delay 1 --retry-all-errors --retry-connrefused -o /dev/null -f "http://$control/healthz" ||
  fail "the peers did not answer: $(tail -n 5 "$work/peers.log" | tr '\n' ' ')"
[ -s "$work/peers.env" ] || fail "the peers wrote no settings"
# The settings: the example with the peers' values, this smoke's code,
# the client secret named, and short periods so the status lines and the
# schema refusal come within the smoke.
overrides=(USSP_SYSTEM_ID USSP_TOKEN_CLIENT_SECRET_FILE USSP_STATUS_INTERVAL_S USSP_SCHEMA_WAIT_S)
mapfile -t peer_keys < <(sed 's/=.*//' "$work/peers.env")
pattern="^($(IFS='|'; echo "${overrides[*]}|${peer_keys[*]}"))="
pattern="${pattern// /|}"
{
  grep -vE "$pattern" "$deploy/compose/ussp.env.example"
  echo "USSP_SYSTEM_ID=$code"
  echo "USSP_TOKEN_CLIENT_SECRET_FILE=/run/secrets/ussp/client.secret"
  echo "USSP_STATUS_INTERVAL_S=5"
  echo "USSP_SCHEMA_WAIT_S=5"
  cat "$work/peers.env"
} > "$work/settings.env"
cat > "$work/deploy.env" <<EOF
USSP_GO_IMAGE=$go_image
USSP_WEB_IMAGE=$web_image
USSP_HOST=$host
AUTHORITY_HOST=authority.smoke.invalid
CISP_HOST=cisp.smoke.invalid
ANSP_HOST=ansp.smoke.invalid
LAB_HOST=lab.smoke.invalid
USSP_SETTINGS_FILE=$work/settings.env
USSP_SECRETS_DIR=$work/secrets
USSP_GROUND_DIR=$work/ground
USSP_EDGE_NETWORK=$edge
USSP_CADDY_IP=$prefix.2
USSP_WEB_IP=$prefix.3
USSP_CA_BUNDLE=/etc/smoke/ca.pem
SMOKE_DIR=$work
SMOKE_HTTPS_PORT=$port
SMOKE_PEERS_IP=$peers_ip
EOF
echo "smoke: peers up ($(grep -c . "$work/peers.env") settings), $(grep -c . "$work/settings.env") settings lines"
out="$(dc up -d --no-deps --wait --wait-timeout 60 caddy 2>&1)" ||
  fail "the edge Caddy did not become healthy: $(tail -n 3 <<<"$out" | tr '\n' ' ') $(dc logs --no-log-prefix --tail 5 caddy 2>&1 | tr '\n' ' ')"
echo "smoke: edge Caddy healthy on 127.0.0.1:$port"

deploy_args=(--env-file "$work/deploy.env" --project "$project" -f "$here/compose.smoke.yaml")
export USSP_EXPECT_VERSION="${SMOKE_VERSION:-}"
# What the smoke cannot have, accepted by name, each printed by deploy.sh:
#   api:operating_status   no start notice is sent before an admin
#                          confirms the start on the console (Art. 7(6))
#   <process>:policy       no policy version is stored before an admin
#                          publishes the first one from the console; the
#                          defaults apply (pending GCAA)
#   monitor:adsb_rx        no e-conspicuity receiver (USSP_ADSB_SOURCE)
#   monitor:terrain        no pinned terrain source is published yet
accepts=(--accept api:operating_status --accept monitor:adsb_rx --accept monitor:terrain)
for s in telemetry-ingest rid-sp monitor traffic-ws tsdb-writer; do accepts+=(--accept "$s:policy"); done

# ---- 3 -----------------------------------------------------------------------
step "3. deploy.sh (local images, strict)"
"$deploy/deploy.sh" "${deploy_args[@]}" --unsigned-local-images --strict "${accepts[@]}"

# ---- 4 -----------------------------------------------------------------------
step "4. the verification without the acceptances fails and names them"
rc=0
out="$("$deploy/deploy.sh" "${deploy_args[@]}" --unsigned-local-images --verify-only --strict 2>&1)" || rc=$?
printf '%s\n' "$out" | grep -E 'FAIL|ok, every' || true
if [ "$rc" -eq 0 ] || ! grep -q 'FAIL api: /readyz operating_status' <<<"$out"; then
  fail "deploy.sh --verify-only --strict exited $rc without naming api's operating_status"
fi
echo "smoke: refused as it should be (exit $rc, api's operating_status named)"

# ---- 5 -----------------------------------------------------------------------
step "5. the simulated operator through the public host"
driver() { # <phase>
  local rc=0
  STAGING_SMOKE=1 STAGING_SMOKE_PHASE="$1" STAGING_SMOKE_URL="https://$host" STAGING_SMOKE_CONNECT="127.0.0.1:$port" \
    STAGING_SMOKE_CA_FILE="$work/ca/ca.pem" STAGING_SMOKE_STATE_DIR="$work/state" STAGING_SMOKE_CONTROL="$control" \
    "$smoke_test" -test.run '^TestStagingSmoke$' -test.v -test.count 1 > "$work/driver-$1.log" 2>&1 || rc=$?
  cat "$work/driver-$1.log"
  [ "$rc" -eq 0 ] || fail "the driver ($1) exited $rc"
  if grep -q -- '--- SKIP' "$work/driver-$1.log"; then fail "the driver ($1) skipped with STAGING_SMOKE=1"; fi
  grep -q -- '--- PASS: TestStagingSmoke' "$work/driver-$1.log" || fail "the driver ($1) did not report PASS"
}
driver file

# ---- 6 -----------------------------------------------------------------------
step "6. backup and restore check"
pg_q() { dc exec -T timescaledb psql -X -v ON_ERROR_STOP=1 -q -U postgres -d "$1" -tAc "$2" | tr -d '\r'; }
# One telemetry row from before the time-series window, written by the
# database's clock: the backup must leave its data out (the absence),
# beside the operator's rows it keeps (the presence).
pg_q ussp_timeseries "INSERT INTO telemetry (flight_id, captured_at, rx_ts, time_source, geom, cell5)
  VALUES (gen_random_uuid(), now() - interval '10 days', now() - interval '10 days', 'gnss',
          ST_SetSRID(ST_MakePoint(44.8, 41.7), 4326), 'c5:smoke')" >/dev/null
# The writers paused, so nothing changes between the counts and the dump.
writers=(api telemetry-ingest rid-sp monitor traffic-ws dss-sync tsdb-writer)
dc pause "${writers[@]}" >/dev/null 2>&1
# A held flight from before the window: an occurrence report (delivered,
# so nothing sends it) names it, and its two telemetry rows 10 days back
# must come back from the restore whatever their age (the presence),
# beside the unheld row above that must not (the absence).
held_id="$(pg_q ussp_relational "SELECT gen_random_uuid()")"
pg_q ussp_relational "INSERT INTO occurrence_reports (report_ref, kind, became_aware_at, deadline_at, payload, source_kind, source_ref,
    channel, flagged_by, reporter_ref, flight_ids, state, submitted_at, authority_ref)
  VALUES ('smoke-held', 'other', now(), now() + interval '72 hours', '{}', 'flight', '$held_id',
    'voluntary', 'supervisor', 'system', ARRAY['$held_id'::uuid], 'delivered', now(), 'smoke')" >/dev/null
pg_q ussp_timeseries "INSERT INTO telemetry (flight_id, captured_at, ts, rx_ts, time_source, geom, cell5)
  SELECT '$held_id', now() - d * interval '1 day', now() - d * interval '1 day', now() - d * interval '1 day', 'gnss',
         ST_SetSRID(ST_MakePoint(44.8, 41.7), 4326), 'c5:smoke'
  FROM (VALUES (10), (11)) AS v(d)" >/dev/null
# Their chunks compressed, as telemetry's compression policy leaves every
# chunk older than 7 days on a host that has run for a week: the held
# export reads them from compressed chunks and the restore check writes
# them back into one (the path a real backup takes, not only the
# uncompressed one). Every chunk holding a held row must be compressed.
pg_q ussp_timeseries "SELECT count(compress_chunk(c, if_not_compressed => true))
  FROM show_chunks('telemetry', older_than => interval '7 days') AS c" >/dev/null
held_chunks="$(pg_q ussp_timeseries "SELECT count(*) || ' ' || coalesce(bool_and(ch.is_compressed), false)
  FROM (SELECT DISTINCT tableoid::regclass::text AS c FROM telemetry WHERE flight_id = '$held_id') AS h
  JOIN timescaledb_information.chunks ch ON format('%I.%I', ch.chunk_schema, ch.chunk_name) = h.c")"
[ "$held_chunks" = "2 true" ] || fail "the held flight's two chunks are not both compressed before the backup ($held_chunks)"
echo "smoke: the held flight's 2 chunks before the window are compressed"
recent="$(pg_q ussp_timeseries "SELECT count(*) FROM telemetry WHERE captured_at >= now() - interval '7 days'")"
held_old="$(pg_q ussp_timeseries "SELECT count(*) FROM telemetry WHERE flight_id = '$held_id' AND captured_at < now() - interval '7 days'")"
all_rows="$(pg_q ussp_timeseries "SELECT count(*) FROM telemetry")"
bundles="$(pg_q ussp_relational "SELECT count(*) FROM record_bundles")"
held_flights="$(pg_q ussp_relational "SELECT count(DISTINCT f) FROM occurrence_reports, unnest(flight_ids) AS f")"
expect="events=$(pg_q ussp_relational "SELECT count(*) FROM events"),operator_accounts=$(pg_q ussp_relational "SELECT count(*) FROM operator_accounts"),operational_intents=$(pg_q ussp_relational "SELECT count(*) FROM operational_intents"),telemetry=$((recent + held_old))"
echo "smoke: live counts with the writers paused: $expect (telemetry $all_rows rows in all, $recent in the window, $held_old of the held flight before it; $bundles record bundles)"
rc=0
USSP_COMPOSE_PROJECT="$project" USSP_SECRETS_DIR="$work/secrets" "$deploy/backup.sh" --settings "$work/settings.env" "$work/backups" || rc=$?
dc unpause "${writers[@]}" >/dev/null 2>&1
[ "$rc" -eq 0 ] || fail "backup.sh exited $rc"
if [ "$all_rows" -le $((recent + held_old)) ] || [ "$recent" -eq 0 ] || [ "$held_old" -ne 2 ]; then
  fail "the smoke's telemetry is not inside the window, held before it and unheld before it ($recent, $held_old held, of $all_rows)"
fi
# The api built the bundles of the days before the smoke at its start
# (records.Daily): there are some to back up.
[ "$bundles" -gt 0 ] || fail "no record bundle was built before the backup: the records archive would be checked empty"
# The backup's NATS user reads every bucket (the export above) and writes
# none: a put as that user is refused by the server.
rc=0
out="$(MSYS_NO_PATHCONV=1 docker run --rm --network "${project}_ussp" --env-file "$work/secrets/backup.env" \
  natsio/nats-box:0.18.0@sha256:abdc9f9f0120bb8adfbf674eb037d1551db55356eb198b7bd4ffed377f6950a6 \
  sh -c 'nats --server="$NATS_URL" --timeout=3s kv put policy current smoke' 2>&1)" || rc=$?
if [ "$rc" -eq 0 ] || ! grep -q 'Permissions Violation for Publish to "$KV.policy.current"' <<<"$out"; then
  fail "the backup user could put a key (exit $rc): $(tail -n 1 <<<"$out")"
fi
echo "smoke: the backup user reads the buckets and its put is refused (Permissions Violation)"
# It reads the KV streams only, and deletes no consumer: TRK's info and
# a consumer deletion are refused by the server (the presence of the
# narrowing; the export above is its twin).
for probe in 'stream info TRK|$JS.API.STREAM.INFO.TRK' 'consumer rm TRK smoke -f|$JS.API.CONSUMER.DELETE.TRK.smoke'   'consumer rm KV_policy smoke -f|$JS.API.CONSUMER.DELETE.KV_policy.smoke'; do
  cmd="${probe%%|*}"; subject="${probe#*|}"
  rc=0
  out="$(MSYS_NO_PATHCONV=1 docker run --rm --network "${project}_ussp" --env-file "$work/secrets/backup.env" -e CMD="$cmd"     natsio/nats-box:0.18.0@sha256:abdc9f9f0120bb8adfbf674eb037d1551db55356eb198b7bd4ffed377f6950a6     sh -c 'nats --server="$NATS_URL" --timeout=3s $CMD' 2>&1)" || rc=$?
  if [ "$rc" -eq 0 ] || ! grep -qF "Permissions Violation for Publish to \"$subject\"" <<<"$out"; then
    fail "the backup user's '$cmd' was not refused (exit $rc): $(tail -n 1 <<<"$out")"
  fi
  echo "smoke: the backup user's '$cmd' is refused (Permissions Violation for $subject)"
done
rc=0
out="$(USSP_COMPOSE_PROJECT="$project" RESTORE_CHECK_EXPECT="$expect" "$deploy/restore-check.sh" "$work/backups" 2>&1)" || rc=$?
printf '%s\n' "$out"
[ "$rc" -eq 0 ] || fail "restore-check exited $rc"
grep -q "held_flights=$held_flights held_rows=$held_old record_bundles=$bundles (archive $bundles)" <<<"$out" ||
  fail "restore-check did not give back the held flight's $held_old rows and the $bundles record bundles"
echo "smoke: the held flight's $held_old rows before the window came back, the unheld one did not, and the $bundles record bundles"
events="${expect#events=}"
events="${events%%,*}"
wrong="events=$((events + 1)),${expect#*,}"
rc=0
out="$(USSP_COMPOSE_PROJECT="$project" RESTORE_CHECK_EXPECT="$wrong" "$deploy/restore-check.sh" "$work/backups" 2>&1)" || rc=$?
if [ "$rc" -eq 0 ] || ! grep -q "expected $wrong" <<<"$out"; then
  fail "restore-check accepted counts that differ ($wrong)"
fi
echo "smoke: restore-check refuses counts that differ (exit $rc)"
cp -r "$work/backups" "$work/tampered"
dump="$(find "$work/tampered" -name 'ussp_relational-*.dump' | head -n 1)"
printf 'x' >> "$dump"
rc=0
out="$(USSP_COMPOSE_PROJECT="$project" "$deploy/restore-check.sh" "$work/tampered" 2>&1)" || rc=$?
if [ "$rc" -eq 0 ] || ! grep -q 'the manifest says otherwise' <<<"$out"; then
  fail "restore-check accepted a dump that differs from its manifest: $(tail -n 1 <<<"$out")"
fi
echo "smoke: restore-check refuses a dump that differs from its manifest (exit $rc)"
# A manifest whose KV export held another number of buckets than the one
# kept (a bucket lost between the export and the check) is refused; the
# files themselves still match it.
cp -r "$work/backups" "$work/fewer-buckets"
m="$(find "$work/fewer-buckets" -name 'manifest-*.json' | sort -r | head -n 1)"
jq '.kv.buckets_with_keys += 1' "$m" >"$m.tmp" && mv "$m.tmp" "$m"
rc=0
out="$(USSP_COMPOSE_PROJECT="$project" "$deploy/restore-check.sh" "$work/fewer-buckets" 2>&1)" || rc=$?
if [ "$rc" -eq 0 ] || ! grep -q 'buckets, the backup recorded' <<<"$out"; then
  fail "restore-check accepted a KV export with another bucket count than its manifest: $(tail -n 1 <<<"$out")"
fi
echo "smoke: restore-check refuses a KV export whose bucket count differs from its manifest (exit $rc)"
# A hypertable with a flight_id the held export does not read stops the
# backup before it writes anything (its held rows would be lost without
# a word); the backup above, without one, is the twin.
pg_q ussp_timeseries "CREATE TABLE smoke_flight_rows (flight_id uuid NOT NULL, at timestamptz NOT NULL);
  SELECT create_hypertable('smoke_flight_rows', by_range('at', INTERVAL '1 day'))" >/dev/null
rc=0
out="$(USSP_COMPOSE_PROJECT="$project" USSP_SECRETS_DIR="$work/secrets" "$deploy/backup.sh" --settings "$work/settings.env" "$work/refused" 2>&1)" || rc=$?
pg_q ussp_timeseries "DROP TABLE smoke_flight_rows" >/dev/null
if [ "$rc" -eq 0 ] || ! grep -q 'flight_id the held export does not read: smoke_flight_rows' <<<"$out"; then
  fail "backup.sh did not refuse a hypertable of flights it does not read (exit $rc): $(tail -n 1 <<<"$out")"
fi
left="$(find "$work/refused" -mindepth 1 -maxdepth 1 -printf '%f ')"
[ -z "$left" ] || fail "the refused backup left files: $left"
echo "smoke: backup.sh refuses a hypertable of flights the held export does not read, and writes nothing (exit $rc)"

# ---- 7 -----------------------------------------------------------------------
step "7. the stack removed (volumes kept), deployed again, read back"
dc down --remove-orphans >/dev/null 2>&1 || fail "compose down failed"
[ -z "$(docker ps -aq --filter "label=com.docker.compose.project=$project")" ] || fail "containers left after compose down"
echo "smoke: no container of $project left; volumes kept: $(docker volume ls -q --filter "label=com.docker.compose.project=$project" | tr '\n' ' ')"
out="$(dc up -d --no-deps --wait --wait-timeout 60 caddy 2>&1)" || fail "the edge Caddy did not come back"
"$deploy/deploy.sh" "${deploy_args[@]}" --unsigned-local-images --strict "${accepts[@]}" | grep -E '^deploy: (migrate|ok|FAIL)'
driver after-restart

# ---- 8 -----------------------------------------------------------------------
step "8. one migration back: the process refuses and names both versions"
refuses() { # <migrate service> <process> <tree>
  local m="$1" svc="$2" tree="$3" out rc=0 down up
  down="$(dc run --rm --no-deps -T "$m" down 2>&1)" || fail "$m down failed: $(tail -n 2 <<<"$down" | tr '\n' ' ')"
  down="$(jq -cR 'fromjson? | select(.msg == "migrate done")' <<<"$down" | tr -d '\r' | tail -n 1)"
  local from to
  from="$(jq -r .from <<<"$down" | tr -d '\r')"; to="$(jq -r .to <<<"$down" | tr -d '\r')"
  if [ -z "$to" ] || [ "$to" = null ] || [ "$to" -ge "$from" ]; then fail "$m down did not go back ($down)"; fi
  dc stop "$svc" >/dev/null 2>&1
  out="$(timeout 120 docker compose -p "$project" --env-file "$work/deploy.env" --env-file "$work/settings.env" \
    -f "$deploy/compose/staging.yaml" -f "$here/compose.smoke.yaml" run --rm --no-deps -T "$svc" 2>&1)" || rc=$?
  local want="$tree schema is at version $to, this build needs $from"
  if [ "$rc" -eq 0 ] || [ "$rc" -eq 124 ] || ! grep -qF "$want" <<<"$out"; then
    printf '%s\n' "$out" | tail -n 5
    fail "$svc against $tree version $to exited $rc without saying '$want'"
  fi
  echo "smoke: $svc refused (exit $rc): $want"
  up="$(dc run --rm --no-deps -T "$m" 2>&1)" || fail "$m up failed: $(tail -n 2 <<<"$up" | tr '\n' ' ')"
  up="$(jq -cR 'fromjson? | select(.msg == "migrate done")' <<<"$up" | tr -d '\r' | tail -n 1)"
  [ "$(jq -r .to <<<"$up" | tr -d '\r')" = "$from" ] || fail "$m up did not return to $from ($up)"
  dc up -d --no-deps --wait --wait-timeout 120 "$svc" >/dev/null 2>&1 || fail "$svc did not come back after the migration"
  echo "smoke: $m brought $tree back to $from; $svc healthy again"
}
refuses migrate api relational
refuses migrate-timeseries tsdb-writer timeseries
"$deploy/deploy.sh" "${deploy_args[@]}" --unsigned-local-images --verify-only --strict "${accepts[@]}" | grep -E '^deploy: (ok|FAIL)'

# ---- 9 -----------------------------------------------------------------------
step "9. memory and CPU (docker stats, after the smoke)"
mapfile -t ids < <(docker ps -q --filter "label=com.docker.compose.project=$project")
docker stats --no-stream --format '{{.Name}}\t{{.MemUsage}}\t{{.CPUPerc}}' "${ids[@]}" |
  sed "s/^$project-//" | sort | tee "$work/stats.txt"
total="$(awk -F'\t' '{print $2}' "$work/stats.txt" |
  awk '{v=$1; u=v; gsub(/[0-9.]/,"",u); gsub(/[A-Za-z]/,"",v); m=(u=="GiB")?v*1024:(u=="KiB")?v/1024:v; s+=m} END {printf "%.0f", s}')"
echo "smoke: $total MiB in containers"
if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then
  {
    echo "### staging smoke"
    echo
    echo "Every step passed; $total MiB in containers after the smoke."
    echo
    echo '| container | memory | CPU |'
    echo '|---|---|---|'
    awk -F'\t' '{print "| " $1 " | " $2 " | " $3 " |"}' "$work/stats.txt"
  } >> "$GITHUB_STEP_SUMMARY"
fi

echo
echo "smoke: ok, every step passed"
