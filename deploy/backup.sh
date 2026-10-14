#!/usr/bin/env bash
# shellcheck disable=SC2016 # SQL, jq and the container's shell name their own $variables
# Nightly backup of uspace-ussp (docs/PLAN.md §11, WP-20), run by the
# host's cron as the deploy user and followed by the restore check
# (docs/RUNBOOKS/backup.md):
#
#   deploy/backup.sh [--settings <settings file>] <backup directory>
#
# Writes, under one UTC stamp:
#
#   ussp_relational-<stamp>.dump  the relational database whole (accounts,
#                                 intents and decisions, alerts, records,
#                                 occurrences, the audit log), pg_dump -Fc
#   ussp_timeseries-<stamp>.dump  the time-series database with the data of
#                                 the chunks of the last USSP_BACKUP_TS_DAYS
#                                 days only (the spec's uncompressed window,
#                                 05 §4): every table, schema and older chunk
#                                 is in the dump without its rows. The cutoff
#                                 is the database's clock, not the host's
#   ussp_timeseries_held-<stamp>.sql
#                                 the rows the dump leaves out that belong to a
#                                 held flight, whatever their age: every flight
#                                 an occurrence report names (the source of
#                                 record_holds). Its telemetry and conformance
#                                 samples, and the traffic products its client
#                                 was shown while it flew, of every chunk before
#                                 the cutoff, one COPY per chunk, applied with
#                                 psql after the dump's restore
#   records-<stamp>.tar           every daily record bundle record_bundles names,
#                                 whatever its age, from the records volume
#                                 (USSP_RECORDS_DIR of api), each read back
#                                 against its recorded SHA-256
#   nats-kv-<stamp>.jsonl        every JetStream KV bucket, one line per key
#                                 ({"bucket","key","value_b64"}), read as the
#                                 backup's own NATS user (read-only: nats.
#                                 staging.conf)
#   manifest-<stamp>.json         what was written: the database clock, the
#                                 cutoff, chunks kept and left out, each
#                                 file's bytes, SHA-256 and entries, each
#                                 tree's schema version, the held flights and
#                                 their rows per chunk, the bundles by name
#                                 and SHA-256, and the KV export's keys,
#                                 buckets and buckets with keys
#
# The dumps are taken inside the project's timescaledb container (the
# server's own pg_dump). Each file is written under a .partial name and
# read back before it is moved into place: a dump must list with
# pg_restore --list, the held rows must be one closed COPY per chunk,
# the records archive must hold exactly the bundles record_bundles names
# with their SHA-256, the KV export must hold one valid line per key it
# listed. A failed run never leaves a file that looks like a backup, and
# the manifest, written last, is what marks a stamp complete. The
# JetStream streams are not backed up: each is bounded by age and what
# they carry is in the databases.
#
# Off the host: with USSP_BACKUP_REMOTE (an rclone remote and path), each
# file is copied there and the copy's size compared. Which account holds
# it is the owner's open question; without one the run says the files
# stayed on this host.
#
# Then removes this job's complete stamps older than USSP_BACKUP_KEEP_DAYS.
# Both day counts are national choices (spec 05 §4), pending GCAA:
# configuration, read from the settings file when --settings names it
# (USSP_BACKUP_KEEP_DAYS=14, USSP_BACKUP_TS_DAYS=7 by default). A failed
# step exits non-zero and leaves older files alone. Nothing secret is
# printed.
#
# Environment: USSP_COMPOSE_PROJECT (uspace-ussp), USSP_SECRETS_DIR (for
# backup.env, the backup's NATS user), USSP_BACKUP_REMOTE, NATS_BOX_IMAGE;
# USSP_BACKUP_KEEP_DAYS and USSP_BACKUP_TS_DAYS override the settings.
set -euo pipefail

NATS_BOX_IMAGE="${NATS_BOX_IMAGE:-natsio/nats-box:0.18.0@sha256:abdc9f9f0120bb8adfbf674eb037d1551db55356eb198b7bd4ffed377f6950a6}"
settings=""
dir=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --settings) settings="${2:?--settings needs a file}"; shift ;;
    -*) echo "usage: deploy/backup.sh [--settings <file>] <backup directory>" >&2; exit 2 ;;
    *) [ -z "$dir" ] || { echo "backup: one directory" >&2; exit 2; }; dir="$1" ;;
  esac
  shift
done
[ -n "$dir" ] || { echo "usage: deploy/backup.sh [--settings <file>] <backup directory>" >&2; exit 2; }
setting() { # <name> <default>: the environment, then the settings file, then the default
  local v="${!1:-}"
  if [ -z "$v" ] && [ -n "$settings" ]; then
    [ -r "$settings" ] || { echo "backup: cannot read $settings" >&2; exit 2; }
    v="$(sed -n "s/^$1=//p" "$settings" | tail -n 1)"
  fi
  printf '%s' "${v:-$2}"
}
keep_days="$(setting USSP_BACKUP_KEEP_DAYS 14)"
ts_days="$(setting USSP_BACKUP_TS_DAYS 7)"
project="${USSP_COMPOSE_PROJECT:-uspace-ussp}"
remote="${USSP_BACKUP_REMOTE:-}"
secrets="${USSP_SECRETS_DIR:?the secrets directory (backup.env)}"

die() { echo "backup: $*" >&2; exit 1; }
for n in "$keep_days" "$ts_days"; do
  case "$n" in ''|*[!0-9]*|0) echo "backup: USSP_BACKUP_KEEP_DAYS and USSP_BACKUP_TS_DAYS are whole days, at least 1" >&2; exit 2 ;; esac
done
[ -r "$secrets/backup.env" ] || die "$secrets/backup.env is missing (deploy/gen-secrets.sh)"
type -P jq >/dev/null 2>&1 || die "jq not found"
if [ -n "$remote" ] && ! type -P rclone >/dev/null 2>&1; then
  die "USSP_BACKUP_REMOTE is set but rclone is not installed"
fi
mkdir -p "$dir"
jqr() { jq -r "$@" | tr -d '\r'; }
sha() { sha256sum "$1" | cut -d' ' -f1; }

# container <service>: the one running container of that service.
container() {
  local ids
  ids="$(docker ps -q --filter "label=com.docker.compose.project=$project" --filter "label=com.docker.compose.service=$1")"
  [ -n "$ids" ] || die "no running $1 container in compose project $project"
  [ "$(printf '%s\n' "$ids" | wc -l)" -eq 1 ] || die "more than one $1 container in compose project $project"
  printf '%s' "$ids"
}
pg="$(container timescaledb)"
nats="$(container nats)"
# The records volume: the named volume api has at its records directory.
api="$(container api)"
records_vol="$(docker inspect -f '{{range .Mounts}}{{if eq .Destination "/var/lib/ussp/records"}}{{.Type}} {{.Name}}{{end}}{{end}}' "$api")"
case "$records_vol" in
  "volume "?*) records_vol="${records_vol#volume }" ;;
  *) die "api has no named volume at /var/lib/ussp/records (${records_vol:-none}): the record bundles cannot be backed up" ;;
esac
net="$(docker inspect -f '{{range $k, $v := .NetworkSettings.Networks}}{{$k}}{{"\n"}}{{end}}' "$nats" | head -n 1)"
[ -n "$net" ] || die "the nats container is on no network"
psql_q() { # <database> <sql>: tuples only, unaligned
  docker exec "$pg" psql -X -v ON_ERROR_STOP=1 -q -U postgres -d "$1" -tAc "$2" | tr -d '\r'
}

stamp="$(date -u +%Y%m%dT%H%M%SZ)"
files=()
# The database's clock decides what "the last N days" are.
db_now="$(psql_q ussp_timeseries "SELECT to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"')")"
cutoff="$(psql_q ussp_timeseries "SELECT to_char((now() - make_interval(days => $ts_days)) AT TIME ZONE 'UTC', 'YYYY-MM-DD\"T\"HH24:MI:SS\"Z\"')")"
v_rel="$(psql_q ussp_relational "SELECT coalesce(max(version_id), 0) FROM goose_db_version_relational WHERE is_applied")"
v_ts="$(psql_q ussp_timeseries "SELECT coalesce(max(version_id), 0) FROM goose_db_version_timeseries WHERE is_applied")"

# The chunks whose range ends before the cutoff, with their compressed
# relations (TimescaleDB 2.30: compression_settings, the pinned image):
# their rows are left out of the dump, never their definitions.
# One cutoff, as a value, for this and the held export below: both read
# the same chunks.
old_chunks="$(psql_q ussp_timeseries "
  SELECT format('%I.%I', i.chunk_schema, i.chunk_name)
  FROM timescaledb_information.chunks i
  WHERE i.range_end < '$cutoff'::timestamptz
  UNION
  SELECT cs.compress_relid::regclass::text
  FROM _timescaledb_catalog.compression_settings cs
  JOIN timescaledb_information.chunks i ON cs.relid = format('%I.%I', i.chunk_schema, i.chunk_name)::regclass
  WHERE i.range_end < '$cutoff'::timestamptz AND cs.compress_relid IS NOT NULL")"
all_chunks="$(psql_q ussp_timeseries "SELECT count(*) FROM timescaledb_information.chunks")"
excluded=()
while IFS= read -r t; do [ -n "$t" ] && excluded+=("--exclude-table-data=$t"); done <<<"$old_chunks"

# The held rows (internal/records: a flight an occurrence report names is
# held past its retention). The hypertables a flight's record reads, by
# what ties a row to the flight: its flight_id, or its client and the
# flight's window (the traffic products, records.Builder.products). A
# hypertable with a flight_id this list does not name, or a table whose
# time column is not the one named, stops the backup before anything is
# written: rows of a held flight would be left out without a word.
held_tables=(telemetry:captured_at:flight conformance_samples:captured_at:flight traffic_products:at:client)
unread="$(psql_q ussp_timeseries "
  SELECT h.hypertable_name FROM timescaledb_information.hypertables h
  JOIN information_schema.columns c
    ON c.table_schema = h.hypertable_schema AND c.table_name = h.hypertable_name AND c.column_name = 'flight_id'
  WHERE (h.hypertable_schema, h.hypertable_name) NOT IN (('public', 'telemetry'), ('public', 'conformance_samples'))
  ORDER BY 1")"
[ -z "$unread" ] || die "hypertables with a flight_id the held export does not read: $(printf '%s' "$unread" | tr '\n' ' ' | sed 's/ $//'); nothing written"
for spec in "${held_tables[@]}"; do
  IFS=: read -r t col _ <<<"$spec"
  dim="$(psql_q ussp_timeseries "SELECT column_name FROM timescaledb_information.dimensions
    WHERE hypertable_schema = 'public' AND hypertable_name = '$t' AND dimension_number = 1")"
  [ "$dim" = "$col" ] || die "$t is partitioned by ${dim:-nothing}, the held export reads it by $col; nothing written"
done

declare -A entries
dump() { # <database> [pg_dump args...]
  local db="$1"; shift
  local partial="$dir/.$db-$stamp.dump.partial" final="$dir/$db-$stamp.dump"
  if ! docker exec "$pg" pg_dump -Fc -U postgres "$@" "$db" > "$partial"; then
    rm -f "$partial"
    die "pg_dump $db failed; older backups kept"
  fi
  local n
  if ! n="$(docker exec -i "$pg" pg_restore --list < "$partial" | grep -cv '^;')"; then
    rm -f "$partial"
    die "the dump of $db does not list; older backups kept"
  fi
  mv "$partial" "$final"
  entries[$db]="$n"
  echo "backup: $final ($(wc -c < "$final" | tr -d ' ') bytes, $n entries)"
  files+=("$final")
}
dump ussp_relational
dump ussp_timeseries "${excluded[@]}"
echo "backup: time series since $cutoff (database clock $db_now): $all_chunks chunks, the rows of ${#excluded[@]} relations before the cutoff left out"

# The held flights, read after the relational dump (a report is never
# removed, so every flight it names is here): each with the client and
# the window its traffic products are read by (an id without a flights
# row holds its flight_id rows only). COPY text, one flight per line.
held="$(psql_q ussp_relational "COPY (
  SELECT h.id, f.client_id, f.started_at, coalesce(f.ended_at, now())
  FROM (SELECT DISTINCT unnest(flight_ids) AS id FROM occurrence_reports) h
  LEFT JOIN flights f ON f.id = h.id
  ORDER BY h.id) TO STDOUT")"
held_flights="$(grep -c . <<<"$held" || true)"
# The chunks before the cutoff of the held tables, the ones whose rows the
# dump left out; their held rows are read from each chunk itself, so a
# row is in the dump or in this file, never in both.
partial="$dir/.ussp_timeseries_held-$stamp.sql.partial"
final="$dir/ussp_timeseries_held-$stamp.sql"
script="$dir/.held-$stamp.psql.partial"
{
  printf '%s\n' '\set ON_ERROR_STOP on' 'SET client_min_messages = warning;' \
    'CREATE TEMP TABLE backup_held (flight_id uuid PRIMARY KEY, client_id text, started_at timestamptz, ended_at timestamptz);' \
    'COPY backup_held FROM STDIN;'
  [ "$held_flights" -eq 0 ] || printf '%s\n' "$held"
  printf '%s\n' '\.'
  for spec in "${held_tables[@]}"; do
    IFS=: read -r t col key <<<"$spec"
    cols="$(psql_q ussp_timeseries "SELECT string_agg(quote_ident(attname), ', ' ORDER BY attnum) FROM pg_attribute
      WHERE attrelid = 'public.$t'::regclass AND attnum > 0 AND NOT attisdropped")"
    if [ "$key" = flight ]; then
      match="t.flight_id IN (SELECT flight_id FROM backup_held)"
    else
      match="EXISTS (SELECT 1 FROM backup_held h WHERE h.client_id = t.client_id AND t.$col BETWEEN h.started_at AND h.ended_at)"
    fi
    chunks="$(psql_q ussp_timeseries "SELECT format('%I.%I', chunk_schema, chunk_name) FROM timescaledb_information.chunks
      WHERE hypertable_schema = 'public' AND hypertable_name = '$t' AND range_end < '$cutoff'::timestamptz ORDER BY range_end")"
    while IFS= read -r c; do
      [ -n "$c" ] || continue
      [[ "$c" =~ ^_timescaledb_internal\._hyper_[0-9]+_[0-9]+_chunk$ ]] || die "chunk $c is not a chunk name; nothing more written"
      printf "\\\\qecho '-- chunk %s of %s'\n" "$c" "$t"
      printf "\\\\qecho 'COPY public.%s (%s) FROM stdin;'\n" "$t" "$cols"
      printf 'COPY (SELECT %s FROM %s t WHERE %s) TO STDOUT;\n' "$cols" "$c" "$match"
      printf "\\\\qecho '\\\\\\\\.'\n"
    done <<<"$chunks"
  done
} > "$script"
if ! docker exec -i "$pg" psql -X -q -U postgres -d ussp_timeseries < "$script" > "$partial"; then
  rm -f "$script" "$partial"
  die "the held rows of ussp_timeseries could not be read; older backups kept"
fi
rm -f "$script"
# Read back: every COPY closed, its rows counted per chunk.
if ! held_rows="$(tr -d '\r' < "$partial" | awk '
    inside && $0 == "\\." { inside = 0; next }
    inside { n[c]++; next }
    /^-- chunk / { c = $3; n[c] += 0; t[c] = $5; next }
    /^COPY public\.[a-z_]+ \(.*\) FROM stdin;$/ { if (c == "") exit 2; inside = 1; next }
    { exit 3 }
    END { if (inside) exit 4; for (k in n) printf "%s %s %d\n", k, t[k], n[k] }' | sort)"; then
  rm -f "$partial"
  die "the held rows of ussp_timeseries do not read back as one closed COPY per chunk; older backups kept"
fi
mv "$partial" "$final"
held_total="$(awk '{s += $3} END {print s + 0}' <<<"$held_rows")"
echo "backup: $final ($held_flights held flights, $held_total rows in $(grep -c . <<<"$held_rows" || true) chunks before the cutoff)"
files+=("$final")

# The daily record bundles, read after the relational dump (a bundle is
# never removed, so every one it names is here): each name checked
# before it is read, the archive read back file by file against the
# SHA-256 record_bundles holds. From the records volume, read-only, in
# the pinned nats-box: api's image has no shell.
bundles="$(psql_q ussp_relational "SELECT storage_ref || ' ' || content_hash FROM record_bundles ORDER BY date")"
n_bundles=0
while IFS=' ' read -r ref sum; do
  [ -n "$ref" ] || continue
  [[ "$ref" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}-[0-9a-f]{12}\.jsonl\.gz$ ]] || die "record_bundles names $ref, not a bundle file name; nothing more written"
  [[ "$sum" =~ ^[0-9a-f]{64}$ ]] || die "record_bundles holds no SHA-256 for $ref; nothing more written"
  n_bundles=$((n_bundles + 1))
done <<<"$bundles"
partial="$dir/.records-$stamp.tar.partial"
final="$dir/records-$stamp.tar"
check="$dir/.records-$stamp.check"
if [ "$n_bundles" -eq 0 ]; then
  tar -cf - -T /dev/null > "$partial"
elif ! cut -d' ' -f1 <<<"$bundles" | MSYS_NO_PATHCONV=1 docker run --rm -i -v "$records_vol:/records:ro" "$NATS_BOX_IMAGE" \
    tar -C /records -cf - -T - > "$partial"; then
  rm -f "$partial"
  die "the record bundles could not be read from the volume $records_vol; older backups kept"
fi
rm -rf "$check"
mkdir -p "$check"
if ! listed="$(tar -tf - < "$partial" | tr -d '\r' | sort)" || [ "$listed" != "$(cut -d' ' -f1 <<<"$bundles" | grep . | sort || true)" ] ||
   ! tar -xf - -C "$check" < "$partial"; then
  rm -rf "$check" "$partial"
  die "the records archive does not hold exactly the $n_bundles bundles record_bundles names; older backups kept"
fi
while IFS=' ' read -r ref sum; do
  [ -n "$ref" ] || continue
  if [ "$(sha "$check/$ref")" != "$sum" ]; then
    rm -rf "$check" "$partial"
    die "the bundle $ref does not have the SHA-256 record_bundles holds; older backups kept"
  fi
done <<<"$bundles"
rm -rf "$check"
mv "$partial" "$final"
echo "backup: $final ($n_bundles record bundles, each matching its recorded SHA-256)"
files+=("$final")

# The KV export, from the pinned nats-box on the project's network, as the
# backup's own NATS user. A key name never holds a space, so a line that is
# not a key name (the CLI's "No keys found in bucket") is skipped.
partial="$dir/.nats-kv-$stamp.jsonl.partial"
final="$dir/nats-kv-$stamp.jsonl"
# Every nats call's failure fails the export (no listing is read through
# a pipe that would hide it); its last line on stderr is "<keys> <buckets>".
export_kv='set -eu
s="--server=$NATS_URL"
n=0
nb=0
names="$(nats "$s" kv ls --names)"
for b in $names; do
  listing="$(nats "$s" kv ls "$b")"
  nb=$((nb + 1))
  for k in $(printf "%s\n" "$listing" | grep -E "^[-/_=.a-zA-Z0-9]+\$" || true); do
    nats "$s" kv get "$b" "$k" --raw > /tmp/value
    v="$(base64 /tmp/value | tr -d "\n")"
    jq -cn --arg b "$b" --arg k "$k" --arg v "$v" "{bucket: \$b, key: \$k, value_b64: \$v}"
    n=$((n + 1))
  done
done
echo "$n $nb" >&2'
if ! keys="$(MSYS_NO_PATHCONV=1 docker run --rm --network "$net" --env-file "$secrets/backup.env" \
    "$NATS_BOX_IMAGE" sh -c "$export_kv" 2>&1 > "$partial")"; then
  rm -f "$partial"
  die "the NATS KV export failed: $(printf '%s' "$keys" | tail -n 1); older backups kept"
fi
last="$(printf '%s' "$keys" | tail -n 1 | tr -d '\r')"
[[ "$last" =~ ^([0-9]+)\ ([0-9]+)$ ]] || { rm -f "$partial"; die "the NATS KV export did not say what it wrote ($last); older backups kept"; }
keys="${BASH_REMATCH[1]}"
buckets="${BASH_REMATCH[2]}"
# A running USSP always has its KV buckets (every process ensures the
# topology): none read is a failure, never an empty backup.
[ "$buckets" -gt 0 ] || { rm -f "$partial"; die "the NATS KV export found no bucket; older backups kept"; }
lines="$(wc -l < "$partial" | tr -d ' ')"
if [ "$lines" != "$keys" ] || ! jq -e -s 'all(.[]; has("bucket") and has("key") and has("value_b64"))' "$partial" >/dev/null; then
  rm -f "$partial"
  die "the NATS KV export holds $lines valid lines for $keys keys; older backups kept"
fi
# The buckets the export holds a line of: the restore check counts them
# from the file (an empty bucket has no line, so it cannot count those).
with_keys="$(jq -s 'map(.bucket) | unique | length' "$partial")"
mv "$partial" "$final"
echo "backup: $final ($lines keys in $buckets buckets, $with_keys of them with keys)"
files+=("$final")

# The manifest, last: a stamp without one is incomplete.
partial="$dir/.manifest-$stamp.json.partial"
printf '%s\n' "$held_rows" > "$partial.held"
printf '%s\n' "$bundles" > "$partial.records"
{
  jq -n --arg stamp "$stamp" --arg db_now "$db_now" --arg cutoff "$cutoff" --argjson ts_days "$ts_days" \
    --argjson all "$all_chunks" --argjson out "$(grep -c . <<<"$old_chunks" || true)" \
    --argjson v_rel "$v_rel" --argjson v_ts "$v_ts" --argjson kv "$lines" --argjson buckets "$buckets" --argjson with_keys "$with_keys" \
    --argjson held_flights "$held_flights" --rawfile held_rows "$partial.held" --rawfile bundles "$partial.records" \
    'def lines: split("\n") | map(select(length > 0) | split(" "));
     {stamp: $stamp, db_now: $db_now, ts_cutoff: $cutoff, ts_days: $ts_days,
      chunks: {all: $all, relations_left_out: $out}, schema: {relational: $v_rel, timeseries: $v_ts},
      held: {flights: $held_flights,
             chunks: ($held_rows | lines | map({key: .[0], value: {table: .[1], rows: (.[2] | tonumber)}}) | from_entries)},
      records: {bundles: ($bundles | lines | map({ref: .[0], sha256: .[1]}))},
      kv: {keys: $kv, buckets: $buckets, buckets_with_keys: $with_keys}, files: {}}'
} > "$partial.base"
rm -f "$partial.held" "$partial.records"
cp "$partial.base" "$partial"
for f in "${files[@]}"; do
  b="$(basename "$f")"
  n="null"
  case "$b" in
    ussp_relational-*) n="${entries[ussp_relational]}" ;;
    ussp_timeseries-*) n="${entries[ussp_timeseries]}" ;;
    ussp_timeseries_held-*) n="$held_total" ;;
    records-*) n="$n_bundles" ;;
    nats-kv-*) n="$lines" ;;
  esac
  jq --arg f "$b" --argjson bytes "$(wc -c < "$f" | tr -d ' ')" --arg sha "$(sha "$f")" --argjson n "$n" \
    '.files[$f] = {bytes: $bytes, sha256: $sha, entries: $n}' "$partial" > "$partial.next"
  mv "$partial.next" "$partial"
done
rm -f "$partial.base"
jq -e '.files | length == 5' "$partial" >/dev/null || { rm -f "$partial"; die "the manifest does not name the five files"; }
mv "$partial" "$dir/manifest-$stamp.json"
files+=("$dir/manifest-$stamp.json")
echo "backup: $dir/manifest-$stamp.json (schema relational $v_rel, timeseries $v_ts)"

for f in "${files[@]}"; do
  [ -n "$remote" ] || break
  bytes="$(wc -c < "$f" | tr -d ' ')"
  rclone copyto "$f" "$remote/$(basename "$f")"
  copied="$(rclone size --json "$remote/$(basename "$f")" | jqr .bytes)"
  [ "$copied" = "$bytes" ] || die "the copy of $f at $remote has ${copied:-no} bytes, not $bytes"
  echo "backup: copied to $remote/$(basename "$f") ($copied bytes)"
done
if [ -z "$remote" ]; then
  echo "backup: USSP_BACKUP_REMOTE is not set; the files stayed on this host"
fi

# Rotation: only complete stamps of this job, older than keep_days.
find "$dir" -maxdepth 1 -type f -name 'manifest-*.json' -mtime +"$keep_days" | while IFS= read -r m; do
  s="$(basename "$m" .json)"; s="${s#manifest-}"
  rm -f "$dir/ussp_relational-$s.dump" "$dir/ussp_timeseries-$s.dump" "$dir/ussp_timeseries_held-$s.sql" \
    "$dir/records-$s.tar" "$dir/nats-kv-$s.jsonl" "$m"
  echo "backup: rotated out $s"
done
echo "backup: done ($stamp, kept $keep_days days, time series $ts_days days)"
