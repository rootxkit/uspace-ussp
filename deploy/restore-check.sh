#!/usr/bin/env bash
# shellcheck disable=SC2016 # jq programs name their own $variables
# Restores the newest complete backup of deploy/backup.sh into scratch
# databases and says what came back (WP-20; a restore that was never
# rehearsed is not a backup):
#
#   deploy/restore-check.sh <backup directory>
#
# The newest stamp with a manifest is checked file by file against it
# (bytes and SHA-256), then restored inside the project's timescaledb
# container (the server's own pg_restore) into ussp_relational_restore_check
# and ussp_timeseries_restore_check, created for the check and dropped
# after it, also on failure, and their absence checked; the time series
# through timescaledb_pre_restore() and timescaledb_post_restore(). Then it
# counts what came back: the audit events, the operators, the intents,
# the telemetry rows, the hypertables, each tree's schema version, and
# decodes every KV value of the export. It never says "ok" without the
# numbers (E-02).
#
# It fails when a file differs from the manifest, when a restore fails,
# when a restored schema version is not the one the manifest recorded,
# when the KV export holds another number of keys or of buckets than the
# manifest recorded, when a KV value does not decode, or, with RESTORE_CHECK_EXPECT set
# ("events=N,operator_accounts=M,operational_intents=K,telemetry=T", the
# counts read from the live databases when the dump was taken), when a
# count differs.
#
# The scratch databases live in the production container for the length
# of the check: about the size of the databases again, on the same disk.
#
# Environment: USSP_COMPOSE_PROJECT (uspace-ussp), RESTORE_CHECK_EXPECT.
set -euo pipefail

dir="${1:?usage: deploy/restore-check.sh <backup directory>}"
project="${USSP_COMPOSE_PROJECT:-uspace-ussp}"
expect="${RESTORE_CHECK_EXPECT:-}"
rel=ussp_relational_restore_check
ts=ussp_timeseries_restore_check

die() { echo "restore-check: FAIL $*" >&2; exit 1; }
jqr() { jq -r "$@" | tr -d '\r'; }
type -P jq >/dev/null 2>&1 || die "jq not found"

pg="$(docker ps -q --filter "label=com.docker.compose.project=$project" --filter label=com.docker.compose.service=timescaledb)"
[ -n "$pg" ] || die "no running timescaledb container in compose project $project"
[ "$(printf '%s\n' "$pg" | wc -l)" -eq 1 ] || die "more than one timescaledb container in compose project $project"

manifest="$(find "$dir" -maxdepth 1 -type f -name 'manifest-*.json' | sort -r | head -n 1)"
[ -n "$manifest" ] || die "no complete backup (a manifest-<stamp>.json) in $dir"
stamp="$(jqr .stamp "$manifest")"
echo "restore-check: backup $stamp from $dir"
for f in $(jqr '.files | keys[]' "$manifest"); do
  [ -f "$dir/$f" ] || die "$f is missing"
  bytes="$(wc -c < "$dir/$f" | tr -d ' ')"
  sum="$(sha256sum "$dir/$f" | cut -d' ' -f1)"
  [ "$bytes" = "$(jqr --arg f "$f" '.files[$f].bytes' "$manifest")" ] || die "$f has $bytes bytes, the manifest says otherwise"
  [ "$sum" = "$(jqr --arg f "$f" '.files[$f].sha256' "$manifest")" ] || die "$f does not have the SHA-256 the manifest recorded"
done
echo "restore-check: $(jqr '.files | length' "$manifest") files match the manifest (bytes, SHA-256)"

psql_q() { # <database> <sql>: one value
  docker exec -e PGOPTIONS='-c client_min_messages=warning' "$pg" psql -v ON_ERROR_STOP=1 -X -q -U postgres -d "$1" -tAc "$2" | tr -d '\r'
}
drop_scratch() {
  psql_q postgres "DROP DATABASE IF EXISTS $rel WITH (FORCE)" >/dev/null
  psql_q postgres "DROP DATABASE IF EXISTS $ts WITH (FORCE)" >/dev/null
}
# shellcheck disable=SC2317 # run by the EXIT trap
cleanup() {
  local rc=$?
  if ! drop_scratch; then
    echo "restore-check: FAIL the scratch databases could not be dropped" >&2
    rc=1
  elif [ -n "$(psql_q postgres "SELECT datname FROM pg_database WHERE datname IN ('$rel', '$ts')")" ]; then
    echo "restore-check: FAIL a scratch database is still present after the drop" >&2
    rc=1
  fi
  exit "$rc"
}
trap cleanup EXIT

drop_scratch
psql_q postgres "CREATE DATABASE $rel" >/dev/null
psql_q postgres "CREATE DATABASE $ts" >/dev/null

# --exit-on-error: a restore that half-worked is a failure, not a count.
if ! out="$(docker exec -i "$pg" pg_restore --exit-on-error -U postgres -d "$rel" < "$dir/ussp_relational-$stamp.dump" 2>&1)"; then
  die "pg_restore of ussp_relational-$stamp.dump: $(printf '%s' "$out" | tail -n 3 | tr '\n' ' ')"
fi
psql_q "$ts" "CREATE EXTENSION IF NOT EXISTS timescaledb" >/dev/null
psql_q "$ts" "SELECT timescaledb_pre_restore()" >/dev/null
# The dump creates the extensions it was taken with; the one created above
# is that extension, so its CREATE EXTENSION is the one error the
# TimescaleDB restore procedure expects. Every other error fails.
rc=0
out="$(docker exec -i "$pg" pg_restore -U postgres -d "$ts" < "$dir/ussp_timeseries-$stamp.dump" 2>&1)" || rc=$?
psql_q "$ts" "SELECT timescaledb_post_restore()" >/dev/null
if [ "$rc" -ne 0 ]; then
  others="$(printf '%s\n' "$out" | grep -E '^pg_restore: error:' | grep -v 'extension "timescaledb" already exists' || true)"
  [ -z "$others" ] || die "pg_restore of ussp_timeseries-$stamp.dump: $(printf '%s' "$others" | head -n 3 | tr '\n' ' ')"
fi

events="$(psql_q "$rel" "SELECT count(*) FROM events")"
operators="$(psql_q "$rel" "SELECT count(*) FROM operator_accounts")"
intents="$(psql_q "$rel" "SELECT count(*) FROM operational_intents")"
telemetry="$(psql_q "$ts" "SELECT count(*) FROM telemetry")"
hypertables="$(psql_q "$ts" "SELECT count(*) FROM timescaledb_information.hypertables")"
v_rel="$(psql_q "$rel" "SELECT coalesce(max(version_id), 0) FROM goose_db_version_relational WHERE is_applied")"
v_ts="$(psql_q "$ts" "SELECT coalesce(max(version_id), 0) FROM goose_db_version_timeseries WHERE is_applied")"

kv="$dir/nats-kv-$stamp.jsonl"
kv_lines="$(wc -l < "$kv" | tr -d ' ')"
kv_buckets="$(jqr .bucket "$kv" | sort -u | grep -c . || true)"
if ! jqr .value_b64 "$kv" | while IFS= read -r v; do printf '%s' "$v" | base64 -d >/dev/null || exit 1; done; then
  die "a value of nats-kv-$stamp.jsonl does not decode"
fi

echo "restore-check: restored $stamp: events=$events operator_accounts=$operators operational_intents=$intents telemetry=$telemetry" \
  "hypertables=$hypertables schema relational=$v_rel timeseries=$v_ts nats_kv_keys=$kv_lines nats_kv_buckets_with_keys=$kv_buckets"

[ "$v_rel" = "$(jqr .schema.relational "$manifest")" ] || die "the restored relational schema is at $v_rel, the backup recorded $(jqr .schema.relational "$manifest")"
[ "$v_ts" = "$(jqr .schema.timeseries "$manifest")" ] || die "the restored time-series schema is at $v_ts, the backup recorded $(jqr .schema.timeseries "$manifest")"
[ "$kv_lines" = "$(jqr .kv.keys "$manifest")" ] || die "the KV export holds $kv_lines keys, the backup recorded $(jqr .kv.keys "$manifest")"
# The export has a line for every key, so it shows the buckets that held
# one: the backup recorded how many did, and how many it listed.
want_buckets="$(jqr '.kv.buckets_with_keys // empty' "$manifest")"
[ -n "$want_buckets" ] || die "the manifest records no kv.buckets_with_keys (a backup older than this check): its KV export cannot be checked"
[ "$kv_buckets" = "$want_buckets" ] || die "the KV export holds keys of $kv_buckets buckets, the backup recorded $want_buckets"
[ "$want_buckets" -le "$(jqr .kv.buckets "$manifest")" ] || die "the manifest records $want_buckets buckets with keys of $(jqr .kv.buckets "$manifest") exported"
[ "$hypertables" -gt 0 ] || die "the restored time series has no hypertable"
if [ -n "$expect" ]; then
  got="events=$events,operator_accounts=$operators,operational_intents=$intents,telemetry=$telemetry"
  [ "$got" = "$expect" ] || die "counts $got, expected $expect"
  echo "restore-check: the counts equal the live databases' at the dump ($expect)"
fi
