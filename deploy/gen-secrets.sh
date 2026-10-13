#!/usr/bin/env bash
# Writes every password, key and credentialed env file that
# deploy/compose/staging.yaml reads, into one secrets directory on the
# host (USSP_SECRETS_DIR). Run once before the first deploy:
#
#   deploy/gen-secrets.sh [--check] [--owner <uid>] <secrets directory>
#
# It never overwrites a file that exists, so a second run changes
# nothing and a key is replaced only by deleting it first (a rotation:
# docs/RUNBOOKS/deploy.md). It never prints a secret, only file names.
# --check lists what is missing and creates nothing (exit 1 when
# something is). Every directory is 0700 and every file 0600. --owner
# gives keys/ and its files to the user the Go image runs as (65532,
# distroless nonroot), which needs root; without it they stay the
# caller's (a laptop, CI).
#
# The layout is uspace-deploy's (scripts/gen-secrets.sh, ussp/), so
# either tool's directory serves this compose file:
#
#   db.env                  POSTGRES_PASSWORD, USSP_PG_API_PASSWORD,
#                           USSP_PG_TSDB_PASSWORD (the timescaledb container
#                           and initdb/10-ussp.sql)
#   nats.env                one USSP_NATS_<PROCESS>_PASSWORD per process and
#                           USSP_NATS_BACKUP_PASSWORD (the nats container)
#   <process>.env           USSP_NATS_URL with that process's own user; api
#                           also USSP_PG_URL and USSP_TS_URL, tsdb-writer
#                           USSP_TS_URL; nothing else gets a DSN
#   migrate.env             USSP_PG_URL as ussp_api (the relational owner)
#   migrate-timeseries.env  USSP_TS_URL as ussp_tsdb (the time-series owner)
#   backup.env              NATS_URL of the backup user (deploy/backup.sh)
#   web.env                 USSP_WEB_BFF_SECRET (32 random bytes, base64)
#   keys/issuer.pem         RSA 3072, this USSP's token issuer (its JWKS)
#   keys/mfa.key            base64 of 32 bytes, seals staff TOTP secrets
#
# Not written here: certificate.env and keys/client.secret, which come
# from the authority's answer when it issues this USSP's certificate
# (uspace-deploy scripts/ussp-certificate.sh; deploy.sh checks them).
set -euo pipefail

check=0
owner=""
dir=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    --check) check=1 ;;
    --owner) owner="${2:?--owner needs a uid}"; shift ;;
    -*) echo "usage: deploy/gen-secrets.sh [--check] [--owner <uid>] <secrets directory>" >&2; exit 2 ;;
    *) [ -z "$dir" ] || { echo "gen-secrets: one directory" >&2; exit 2; }; dir="$1" ;;
  esac
  shift
done
[ -n "$dir" ] || { echo "usage: deploy/gen-secrets.sh [--check] [--owner <uid>] <secrets directory>" >&2; exit 2; }
case "$owner" in ''|*[!0-9]*) [ -z "$owner" ] || { echo "gen-secrets: --owner takes a numeric uid" >&2; exit 2; } ;; esac
if [ -n "$owner" ] && [ "$check" -eq 0 ] && [ "$(id -u)" -ne 0 ]; then
  echo "gen-secrets: --owner needs root (the key files are given to uid $owner)" >&2
  exit 2
fi
command -v openssl >/dev/null || { echo "gen-secrets: openssl not found" >&2; exit 2; }

procs=(api telemetry-ingest rid-sp monitor traffic-ws dss-sync tsdb-writer)
upper() { printf '%s' "$1" | tr '[:lower:]-' '[:upper:]_'; }
log() { echo "gen-secrets: $*"; }

umask 077
missing=0
created=0

# need <path>: true when the file must be written (and counts it).
need() {
  if [ -e "$1" ]; then return 1; fi
  missing=$((missing + 1))
  if [ "$check" -eq 1 ]; then log "missing: ${1#"$dir"/}"; return 1; fi
  return 0
}

# put <path> <mode>: moves <path>.tmp into place, read back first.
put() {
  [ -s "$1.tmp" ] || { rm -f "$1.tmp"; echo "gen-secrets: $1 came out empty; nothing kept" >&2; exit 1; }
  chmod "$2" "$1.tmp"
  mv "$1.tmp" "$1"
  created=$((created + 1))
  log "wrote ${1#"$dir"/}"
}

# value <file> <name>: the value of NAME= in an env file this script wrote.
value() { sed -n "s/^$2=//p" "$1" | head -n 1; }

hexpw() { openssl rand -hex 24; }

# write_env <path> <line>...
write_env() {
  local path="$1"; shift
  need "$path" || return 0
  printf '%s\n' "$@" > "$path.tmp"
  put "$path" 0600
}

if [ "$check" -eq 0 ]; then
  mkdir -p "$dir/keys"
  chmod 0700 "$dir" "$dir/keys"
fi

write_env "$dir/db.env" \
  "POSTGRES_PASSWORD=$(hexpw)" "USSP_PG_API_PASSWORD=$(hexpw)" "USSP_PG_TSDB_PASSWORD=$(hexpw)"
lines=()
# NATS reads a $VARIABLE of its configuration as a configuration value:
# a password that starts with a digit and goes on with letters does not
# parse ("Expected a top-level value to end"), and the server never
# starts. Each one starts with a letter.
for p in "${procs[@]}" backup; do lines+=("USSP_NATS_$(upper "$p")_PASSWORD=n$(hexpw)"); done
write_env "$dir/nats.env" "${lines[@]}"
unset lines

# The per-process files are derived from db.env and nats.env, so they
# are written only when those exist (a --check run lists them).
if [ -s "$dir/db.env" ] && [ -s "$dir/nats.env" ]; then
  api_pw="$(value "$dir/db.env" USSP_PG_API_PASSWORD)"
  tsdb_pw="$(value "$dir/db.env" USSP_PG_TSDB_PASSWORD)"
  if [ -z "$api_pw" ] || [ -z "$tsdb_pw" ]; then echo "gen-secrets: db.env lacks a password" >&2; exit 1; fi
  rel="postgres://ussp_api:${api_pw}@ussp-timescaledb:5432/ussp_relational?sslmode=disable"
  ts_api="postgres://ussp_api:${api_pw}@ussp-timescaledb:5432/ussp_timeseries?sslmode=disable"
  ts_owner="postgres://ussp_tsdb:${tsdb_pw}@ussp-timescaledb:5432/ussp_timeseries?sslmode=disable"
  for p in "${procs[@]}" backup; do
    pw="$(value "$dir/nats.env" "USSP_NATS_$(upper "$p")_PASSWORD")"
    [ -n "$pw" ] || { echo "gen-secrets: nats.env lacks the $p password" >&2; exit 1; }
    nats="nats://$p:${pw}@ussp-nats:4222"
    case "$p" in
      api) write_env "$dir/$p.env" "USSP_NATS_URL=$nats" "USSP_PG_URL=$rel" "USSP_TS_URL=$ts_api" ;;
      tsdb-writer) write_env "$dir/$p.env" "USSP_NATS_URL=$nats" "USSP_TS_URL=$ts_owner" ;;
      backup) write_env "$dir/backup.env" "NATS_URL=$nats" ;;
      *) write_env "$dir/$p.env" "USSP_NATS_URL=$nats" ;;
    esac
  done
  write_env "$dir/migrate.env" "USSP_PG_URL=$rel"
  write_env "$dir/migrate-timeseries.env" "USSP_TS_URL=$ts_owner"
  unset api_pw tsdb_pw rel ts_api ts_owner pw nats
else
  for p in "${procs[@]}" backup migrate migrate-timeseries; do need "$dir/$p.env" || true; done
fi

write_env "$dir/web.env" "USSP_WEB_BFF_SECRET=$(openssl rand -base64 32)"

if need "$dir/keys/issuer.pem"; then
  openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:3072 -out "$dir/keys/issuer.pem.tmp" 2>/dev/null
  # A key that does not parse is never kept.
  openssl pkey -in "$dir/keys/issuer.pem.tmp" -noout
  put "$dir/keys/issuer.pem" 0600
fi
if need "$dir/keys/mfa.key"; then
  openssl rand -base64 32 > "$dir/keys/mfa.key.tmp"
  put "$dir/keys/mfa.key" 0600
fi

if [ -n "$owner" ] && [ "$check" -eq 0 ]; then
  chown "$owner:$owner" "$dir/keys" "$dir"/keys/*
fi

if [ "$check" -eq 1 ]; then
  if [ "$missing" -gt 0 ]; then log "$missing file(s) missing in $dir"; exit 1; fi
  log "nothing missing in $dir"
  exit 0
fi
log "$created file(s) written, $(( $(find "$dir" -type f | wc -l) - created )) kept, in $dir"
