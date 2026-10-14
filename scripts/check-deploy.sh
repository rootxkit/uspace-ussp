#!/usr/bin/env bash
# The deployment checks (`make check-deploy`, CI job `deploy`; WP-20).
# Reads deploy/compose/staging.yaml back as compose renders it, never
# its text, and fails on the first rule it breaks; then proves that the
# same rules fail on shapes that break them (the laptop compose, and the
# staging shape with one rule broken at a time), so a rule that cannot
# fail is caught too (E-01).
#
#   scripts/check-deploy.sh
#
# Needs docker compose (only the CLI: nothing is started here but the
# pinned shellcheck container), jq and openssl. Nothing secret is
# printed: the rendered compose holds the throwaway secrets gen-secrets.sh writes into a
# temporary directory, and only names and counts are shown.
set -euo pipefail
cd "$(dirname "$0")/.."
repo="$(pwd)"

work="$(mktemp -d)"
# shellcheck disable=SC2317 # run by the EXIT trap
cleanup() { rm -rf "$work"; }
trap cleanup EXIT
if type -P cygpath >/dev/null 2>&1; then
  work="$(cygpath -m "$work")"
  repo="$(cygpath -m "$repo")"
fi

fails=0
oks=0
ok() { echo "check-deploy: ok   $*"; oks=$((oks + 1)); }
bad() { echo "check-deploy: FAIL $*" >&2; fails=$((fails + 1)); }
die() { echo "check-deploy: $*" >&2; exit 1; }
# A Windows jq ends its lines with CRLF.
jqr() { jq -r "$@" | tr -d '\r'; }

for t in docker jq openssl; do type -P "$t" >/dev/null 2>&1 || die "$t not found"; done
docker compose version >/dev/null 2>&1 || die "docker compose not found"

# ---- the secrets and the settings the render uses ----------------------------

out="$(deploy/gen-secrets.sh "$work/secrets")"
again="$(deploy/gen-secrets.sh "$work/secrets")"
if grep -q '^gen-secrets: 0 file(s) written' <<<"$again"; then
  ok "gen-secrets: $(grep -c '^gen-secrets: wrote' <<<"$out") files written, a second run writes none"
else
  bad "gen-secrets: a second run wrote again: $(tail -n 1 <<<"$again")"
fi
if deploy/gen-secrets.sh --check "$work/secrets" >/dev/null; then
  ok "gen-secrets --check: nothing missing"
else
  bad "gen-secrets --check reports a missing file after a full run"
fi
rm "$work/secrets/web.env"
rc=0
deploy/gen-secrets.sh --check "$work/secrets" >/dev/null 2>&1 || rc=$?
if [ "$rc" -eq 1 ] && [ ! -e "$work/secrets/web.env" ]; then
  ok "gen-secrets --check: a missing web.env fails and is not written"
else
  bad "gen-secrets --check with web.env missing: exit $rc"
fi
deploy/gen-secrets.sh "$work/secrets" >/dev/null
# No value is ever printed: every generated password and key line is
# looked for in what the script said.
leaked=0
while IFS= read -r line; do
  v="${line#*=}"
  [ "${#v}" -ge 16 ] || continue
  if grep -qF -- "$v" <<<"$out$again"; then leaked=1; fi
done < <(cat "$work"/secrets/*.env)
if [ "$leaked" -eq 0 ]; then ok "gen-secrets prints no value it wrote"; else bad "gen-secrets printed a value it wrote"; fi
# NATS parses a $VARIABLE as a value: a password starting with a digit
# and going on with letters stops the server at start.
digit="$(grep -cE '^USSP_NATS_[A-Z_]+_PASSWORD=[0-9]' "$work/secrets/nats.env" || true)"
count="$(grep -cE '^USSP_NATS_[A-Z_]+_PASSWORD=' "$work/secrets/nats.env" || true)"
if [ "$count" -eq 8 ] && [ "$digit" -eq 0 ]; then
  ok "gen-secrets: all 8 NATS passwords start with a letter"
else
  bad "gen-secrets: $digit of $count NATS passwords start with a digit (NATS would not start)"
fi
# Modes are not kept on a Windows checkout's file system.
if type -P cygpath >/dev/null 2>&1; then
  echo "check-deploy: SKIPPED file modes (Windows file system)"
elif [ -z "$(find "$work/secrets" -type f ! -perm 600)" ]; then
  ok "gen-secrets: every file is mode 600"
else
  bad "gen-secrets: files not mode 600: $(find "$work/secrets" -type f ! -perm 600 | tr '\n' ' ')"
fi

sed "s#^USSP_SETTINGS_FILE=.*#USSP_SETTINGS_FILE=$repo/deploy/compose/ussp.env.example#; s#^USSP_SECRETS_DIR=.*#USSP_SECRETS_DIR=$work/secrets#; s#^USSP_GROUND_DIR=.*#USSP_GROUND_DIR=$work/ground#" \
  deploy/compose/deploy.env.example > "$work/deploy.env"
mkdir -p "$work/ground"

# render <out json> <compose file> [compose args...]: the rendered model.
render() {
  local json="$1" file="$2"; shift 2
  docker compose -p uspace-ussp --env-file "$work/deploy.env" --env-file "$repo/deploy/compose/ussp.env.example" \
    -f "$file" "$@" config --format json > "$json"
}

# ---- the shape ---------------------------------------------------------------

# shape <json>: every rule of staging.yaml's header against a rendered
# model (scripts/deploy-shape.jq); prints one line per broken rule and
# returns 1 when any is broken.
bounds="$(grep -oE 'USSP_[A-Z_]+_(STREAM|BUCKET)_MAX_BYTES' deploy/ENV.md | sort -u | jq -R . | jq -sc .)"
shape() {
  local findings
  findings="$(jqr --argjson bounds "$bounds" -f scripts/deploy-shape.jq "$1")" || { echo "  the shape rules did not run"; return 1; }
  [ -z "$findings" ] && return 0
  while IFS= read -r line; do echo "  $line"; done <<<"$findings"
  return 1
}

render "$work/staging.json" deploy/compose/staging.yaml
if out="$(shape "$work/staging.json")"; then
  ok "staging.yaml keeps every rule ($(jqr '.services | length' "$work/staging.json") services)"
else
  bad "staging.yaml breaks a rule:"; echo "$out" >&2
fi
total="$(jqr "[.services[].deploy.resources.limits.memory | tonumber] | add" "$work/staging.json")"
reserved="$(jqr --argjson b "$bounds" "[\$b[] as \$v | .services.api.environment[\$v] | tonumber] | add" "$work/staging.json")"
store="$(jqr ".services.nats.environment.NATS_MAX_FILE_STORE" "$work/staging.json")"
echo "check-deploy:      memory limits $((total / 1048576)) MiB in all, one-shots included;" \
  "JetStream bounds $((reserved / 1048576)) MiB of a $((store / 1048576)) MiB store"

# The rules fail on a shape that breaks them: the laptop compose publishes
# ports, sets no limit and pins no image.
cp deploy/compose/.env.example "$work/dev.env"
if docker compose -p uspace-ussp --env-file "$work/dev.env" -f deploy/compose/docker-compose.yml config --format json > "$work/dev.json" 2>/dev/null; then
  if out="$(shape "$work/dev.json")"; then
    bad "the rules pass the laptop compose, which publishes ports and sets no limit"
  elif grep -q 'publishes a port' <<<"$out" && grep -q 'no memory limit' <<<"$out" && grep -q 'not pinned by digest' <<<"$out"; then
    ok "the rules refuse the laptop compose ($(wc -l <<<"$out" | tr -d ' ') findings, ports, limits and pins among them)"
  else
    bad "the rules refuse the laptop compose without naming ports, limits and pins:"; echo "$out" >&2
  fi
else
  bad "the laptop compose does not render"
fi
# One broken rule at a time, on the staging shape itself.
mutate() { # <label> <jq filter> <expected finding>
  jq "$2" "$work/staging.json" > "$work/mutated.json"
  if out="$(shape "$work/mutated.json")"; then
    bad "$1: the rules do not notice"
  elif grep -q -- "$3" <<<"$out"; then
    ok "$1: refused ($3)"
  else
    bad "$1: refused for another reason: $out"
  fi
}
mutate "a range trusted as a proxy" '.services["rid-sp"].environment.USSP_TRUSTED_PROXIES = "172.30.0.0/16"' 'not one host'
mutate "a DSN given to a process that does not open it" '.services.monitor.environment.USSP_PG_URL = "postgres://x"' 'USSP_PG_URL is set'
mutate "a shared NATS user" '.services["traffic-ws"].environment.USSP_NATS_URL = "nats://api:x@ussp-nats:4222"' "as 'api'"
mutate "a bound left at its default" 'del(.services.api.environment.USSP_TRK_STREAM_MAX_BYTES)' 'USSP_TRK_STREAM_MAX_BYTES unset'
mutate "bounds over the store" '.services.nats.environment.NATS_MAX_FILE_STORE = "1073741824"' 'the store holds'
mutate "a /healthz-only health check" '.services.monitor.healthcheck.test = ["CMD","/usr/bin/ussp-monitor","healthcheck"]' 'not /readyz'
mutate "a published port" '.services.api.ports = [{"target": 8080, "published": "8080"}]' 'publishes a port'
mutate "an image by tag" '.services.web.image = "ghcr.io/rootxkit/uspace-ussp-web:latest"' 'not pinned by digest'
mutate "its own JWKS read over plain http" '.services["traffic-ws"].environment.USSP_TOKEN_ISSUERS |= sub("=https://ussp.example.test/"; "=http://ussp-api:8080/")' 'over plain http'
mutate "its own host not mapped to Caddy" 'del(.services["telemetry-ingest"].extra_hosts)' 'is not mapped to the edge Caddy'
mutate "a verifier without its jwks_cache key" 'del(.services["rid-sp"].environment.USSP_JWKS_CACHE_KEY_FILE)' 'USSP_JWKS_CACHE_KEY_FILE unset'
mutate "another process's jwks_cache key" '.services["traffic-ws"].volumes |= map(if (.source // "" | endswith("/keys/jwks-cache-traffic-ws.key")) then .source |= sub("traffic-ws.key$"; "api.key") else . end)' "another process's jwks-cache key"
mutate "a jwks_cache key on a process that verifies nothing" '.services.monitor.environment.USSP_JWKS_CACHE_KEY_FILE = "/run/secrets/jwks-cache.key"' 'verifies no token'

# Every base image the two Dockerfiles build from is pinned by digest: a
# tag can move between two builds of one commit.
unpinned="$(grep -HnE '^FROM ' deploy/Dockerfile deploy/web.Dockerfile | grep -vE '^[^:]+:[0-9]+:FROM [^ ]+@sha256:[0-9a-f]{64}( AS [a-z][a-z-]*)?$' || true)"
if [ -z "$unpinned" ]; then
  ok "every FROM of deploy/Dockerfile and deploy/web.Dockerfile is pinned by digest"
else
  bad "base images not pinned by digest: $(tr '\n' ' ' <<<"$unpinned")"
fi

# ---- the settings ------------------------------------------------------------

# Every variable of ENV.md is named in the settings example (set, or
# commented out, or in its list of what compose and the secrets set);
# nothing it sets is unknown to ENV.md but the backup's own two; nothing
# secret is set in it.
# shellcheck disable=SC2016 # a literal backquote
catalogue="$(grep -oE '^\| `USSP_[A-Z0-9_]+`' deploy/ENV.md | tr -d '|` ' | sort -u)"
missing=""
for v in $catalogue; do grep -qE "(^|[^A-Z0-9_])$v([^A-Z0-9_]|$)" deploy/compose/ussp.env.example || missing+=" $v"; done
if [ -z "$missing" ]; then
  ok "the settings example names all $(wc -w <<<"$catalogue" | tr -d ' ') variables of ENV.md"
else
  bad "the settings example does not name:$missing"
fi
unknown=""
for v in $(grep -oE '^USSP_[A-Z0-9_]+=' deploy/compose/ussp.env.example | tr -d '='); do
  case "$v" in USSP_BACKUP_KEEP_DAYS|USSP_BACKUP_TS_DAYS) continue ;; esac
  grep -qx "$v" <<<"$catalogue" || unknown+=" $v"
done
if [ -z "$unknown" ]; then ok "the settings example sets no variable ENV.md does not know"; else bad "unknown variables set:$unknown"; fi
secret_set="$(grep -oE '^USSP_(PG_URL|TS_URL|NATS_URL|NATS_CREDS|CERTIFICATE_ID|TRUSTED_PROXIES|[A-Z_]*PASSWORD)=' deploy/compose/ussp.env.example || true)"
if [ -z "$secret_set" ]; then ok "the settings example sets no DSN, credential or proxy"; else bad "the settings example sets $secret_set"; fi
code="$(sed -n 's/^USSP_SYSTEM_ID=//p' deploy/compose/ussp.env.example)"
if [[ "$code" =~ ^[A-Z0-9]{1,8}$ ]]; then ok "USSP_SYSTEM_ID=$code is an authority code"; else bad "USSP_SYSTEM_ID=$code is not an authority code (A-Z, 0-9, at most 8)"; fi

# ---- deploy.sh, verify-image.sh and teardown.sh ------------------------------

# Each refusal of the preflight, before anything is pulled or started,
# then the run that gets past it (it fails later, at the probes, since
# nothing runs here: the presence that makes the refusals mean something).
cp -r "$work/secrets" "$work/pre-secrets"
mkdir -p "$work/pre-ground"
printf 'P5\n2 3\n65535\n' > "$work/pre-ground/egm2008-2_5.pgm"
sed "s#^USSP_SECRETS_DIR=.*#USSP_SECRETS_DIR=$work/pre-secrets#; s#^USSP_GROUND_DIR=.*#USSP_GROUND_DIR=$work/pre-ground#; s#^USSP_SETTINGS_FILE=.*#USSP_SETTINGS_FILE=$work/pre-settings.env#" \
  "$work/deploy.env" > "$work/pre.env"
preflight() { # <label> <expected text> [settings sed] [deploy env sed] [deploy.sh args...]
  local label="$1" want="$2" sset="${3:-}" dset="${4:-}" rc=0 out
  shift 4 || shift $#
  sed "${sset:-p;d}" deploy/compose/ussp.env.example > "$work/pre-settings.env"
  [ -n "$sset" ] || cp deploy/compose/ussp.env.example "$work/pre-settings.env"
  sed "${dset:-}" "$work/pre.env" > "$work/pre-run.env"
  out="$(USSP_DEPLOY_WAIT_S=5 deploy/deploy.sh --env-file "$work/pre-run.env" --project uspace-ussp-check --verify-only "$@" 2>&1)" || rc=$?
  if [ "$rc" -ne 0 ] && grep -qF -- "$want" <<<"$out"; then
    ok "deploy.sh: $label (exit $rc: $want)"
  else
    bad "deploy.sh: $label: exit $rc, want '$want': $(tail -n 3 <<<"$out" | tr '\n' ' ')"
  fi
}
preflight "an image by tag is refused" "is not a reference by digest" "" 's#^USSP_GO_IMAGE=.*#USSP_GO_IMAGE=ghcr.io/rootxkit/uspace-ussp:latest#'
preflight "a DSN in the settings is refused" "the settings file sets USSP_PG_URL=" 's#^USSP_LOG_LEVEL=info#USSP_LOG_LEVEL=info\nUSSP_PG_URL=postgres://x#' ""
preflight "a code the authority never issues is refused" "USSP_SYSTEM_ID 'USSP-DEV' is not the authority's form" 's#^USSP_SYSTEM_ID=.*#USSP_SYSTEM_ID=USSP-DEV#' ""
preflight "a missing geoid grid is refused" "is missing or empty" "" "s#^USSP_GROUND_DIR=.*#USSP_GROUND_DIR=$work/ground#"
rm "$work/pre-secrets/backup.env"
preflight "an incomplete secrets directory is refused" "missing: backup.env" "" ""
cp "$work/secrets/backup.env" "$work/pre-secrets/backup.env"
preflight "--unsigned-local-images with a registry image is refused" "a registry image is always verified" "" "" --unsigned-local-images
preflight "a complete preflight passes, then the probes find nothing running" "preflight: renders" "" "" --without-certificate
# The certificate wiring, in the files scripts/ussp-certificate.sh
# (uspace-deploy) writes: certificate.env as "USSP_CERTIFICATE_ID=<id>\n",
# keys/client.secret without a newline.
cert_id=0123456789abcdef0123456789abcdef
secret_line='s#^\# USSP_TOKEN_CLIENT_SECRET_FILE=#USSP_TOKEN_CLIENT_SECRET_FILE=#'
preflight "no certificate is refused and names the script" "scripts/ussp-certificate.sh ANSWER.json" "" ""
preflight "--without-certificate passes the preflight and says so" "certificate none (--without-certificate)" "" "" --without-certificate
printf 'USSP_CERTIFICATE_ID=%s\n' "${cert_id^^}" > "$work/pre-secrets/certificate.env"
preflight "a certificate id not in the authority's form is refused" "certificate.env must hold one line" "" ""
printf 'USSP_CERTIFICATE_ID=%s\n' "$cert_id" > "$work/pre-secrets/certificate.env"
preflight "the certificate id reaches api" "certificate $cert_id" "" ""
printf 'a-client-secret-of-the-authority' > "$work/pre-secrets/keys/client.secret"
preflight "a client secret the settings do not name is refused" "no outgoing call would use it" "" ""
preflight "the client secret named by the settings is read by the four callers" "outgoing calls as ussp-STG01-01" "$secret_line" ""
rm "$work/pre-secrets/keys/client.secret"
preflight "a client secret named but missing is refused" "keys/client.secret is missing or empty" "$secret_line" ""
printf 'a-client-secret-of-the-authority' > "$work/pre-secrets/keys/client.secret"

for ref in ghcr.io/rootxkit/uspace-ussp:latest "ghcr.io/other/uspace-ussp@sha256:$(printf '0%.0s' {1..64})"; do
  rc=0
  out="$(deploy/verify-image.sh "$ref" 2>&1)" || rc=$?
  if [ "$rc" -eq 1 ] && grep -qE 'not a reference by digest|not an image of' <<<"$out"; then
    ok "verify-image refuses $ref before cosign runs"
  else
    bad "verify-image with $ref: exit $rc: $out"
  fi
done

# teardown.sh: a project with a container is removed and the absence
# checked; a project with nothing is reported empty.
tproject="uspace-ussp-teardown-check-$$"
MSYS_NO_PATHCONV=1 docker run -d --label "com.docker.compose.project=$tproject" \
  "${NATS_BOX_IMAGE:-natsio/nats-box:0.18.0@sha256:abdc9f9f0120bb8adfbf674eb037d1551db55356eb198b7bd4ffed377f6950a6}" \
  nats-server -p 4222 >/dev/null
if [ -n "$(docker ps -aq --filter "label=com.docker.compose.project=$tproject")" ] &&
   out="$(deploy/teardown.sh --project "$tproject" --volumes)" &&
   [ -z "$(docker ps -aq --filter "label=com.docker.compose.project=$tproject")" ]; then
  ok "teardown.sh removed a running container of its project and says so ($out)"
else
  bad "teardown.sh: ${out:-no output}"
  docker rm -f "$(docker ps -aq --filter "label=com.docker.compose.project=$tproject")" >/dev/null 2>&1 || true
fi

# ---- restore-check.sh: the KV export against the topology -------------------

# The number of buckets a backup exported is checked against the
# topology's (deploy/nats-kv-buckets.txt), before any database is
# touched: a manifest that records one bucket more than the topology has
# is refused, as is an export with a bucket the topology does not name;
# the backup that matches gets past the check and stops at the next step
# (no timescaledb container runs here: the presence that makes the
# refusals mean something).
rbk="$work/restore-kv"
mkdir -p "$rbk"
topo_n="$(grep -cvE '^[[:space:]]*(#|$)' deploy/nats-kv-buckets.txt)"
first_bucket="$(grep -vE '^[[:space:]]*(#|$)' deploy/nats-kv-buckets.txt | head -n 1 | tr -d '\r')"
rstamp=20261014T000000Z
kv_backup() { # <dir> <buckets recorded> <bucket of the one key> [records: ok, missing, altered, unrecorded]
  local d="$1" n="$2" b="$3" mode="${4:-ok}" f bytes sum ref
  rm -rf "$d"; mkdir -p "$d/vol"
  printf 'relational' > "$d/ussp_relational-$rstamp.dump"
  printf 'timeseries' > "$d/ussp_timeseries-$rstamp.dump"
  printf -- '-- no held flight\n' > "$d/ussp_timeseries_held-$rstamp.sql"
  jq -cn --arg b "$b" '{bucket: $b, key: "k", value_b64: "dg=="}' > "$d/nats-kv-$rstamp.jsonl"
  # One record bundle, named as the api names it; "altered" changes its
  # bytes after its SHA-256 is recorded, "missing" leaves it out.
  printf 'bundle\n' | gzip -n > "$d/vol/b"
  sum="$(sha256sum "$d/vol/b" | cut -d' ' -f1)"
  ref="2026-10-01-${sum:0:12}.jsonl.gz"
  mv "$d/vol/b" "$d/vol/$ref"
  [ "$mode" != altered ] || printf 'x' >> "$d/vol/$ref"
  if [ "$mode" = missing ]; then
    tar -cf - -T /dev/null > "$d/records-$rstamp.tar"
  else
    (cd "$d/vol" && tar -cf - "$ref") > "$d/records-$rstamp.tar"
  fi
  rm -rf "$d/vol"
  jq -n --arg s "$rstamp" --argjson n "$n" --arg ref "$ref" --arg sum "$sum" '{stamp: $s, schema: {relational: 1, timeseries: 1},
    held: {flights: 0, chunks: {}}, records: {bundles: [{ref: $ref, sha256: $sum}]},
    kv: {keys: 1, buckets: $n, buckets_with_keys: 1}, files: {}}' > "$d/m.json"
  local names=("ussp_relational-$rstamp.dump" "ussp_timeseries-$rstamp.dump" "ussp_timeseries_held-$rstamp.sql" "records-$rstamp.tar" "nats-kv-$rstamp.jsonl")
  if [ "$mode" = unrecorded ]; then
    names=("ussp_relational-$rstamp.dump" "ussp_timeseries-$rstamp.dump" "nats-kv-$rstamp.jsonl")
    jq 'del(.held, .records)' "$d/m.json" > "$d/m2.json" && mv "$d/m2.json" "$d/m.json"
  fi
  for f in "${names[@]}"; do
    bytes="$(wc -c < "$d/$f" | tr -d ' ')"
    sum="$(sha256sum "$d/$f" | cut -d' ' -f1)"
    jq --arg f "$f" --argjson b "$bytes" --arg s "$sum" '.files[$f] = {bytes: $b, sha256: $s}' "$d/m.json" > "$d/m2.json"
    mv "$d/m2.json" "$d/m.json"
  done
  mv "$d/m.json" "$d/manifest-$rstamp.json"
}
restore_kv() { # <label> <expected text> <buckets recorded> <bucket of the key> [records]
  local label="$1" want="$2" rc=0 out
  kv_backup "$rbk/b" "$3" "$4" "${5:-ok}"
  out="$(USSP_COMPOSE_PROJECT="uspace-ussp-restore-check-$$" deploy/restore-check.sh "$rbk/b" 2>&1)" || rc=$?
  if [ "$rc" -ne 0 ] && grep -qF -- "$want" <<<"$out"; then
    ok "restore-check.sh: $label (exit $rc: $want)"
  else
    bad "restore-check.sh: $label: exit $rc, want '$want': $(tail -n 2 <<<"$out" | tr '\n' ' ')"
  fi
}
restore_kv "a backup with one bucket more than the topology is refused" "the topology has $topo_n" "$((topo_n + 1))" "$first_bucket"
restore_kv "a backup with one bucket fewer than the topology is refused" "the topology has $topo_n" "$((topo_n - 1))" "$first_bucket"
restore_kv "a key of a bucket the topology does not name is refused" "not a bucket of the topology" "$topo_n" "not_a_bucket"
# The record bundles, before any database: an archive without the bundle
# the manifest names, or with other bytes for it, is refused, and so is a
# manifest from before the held rows and the bundles were backed up; the
# archive that matches gets past the check (the line below).
restore_kv "a records archive without a bundle the manifest names is refused" "does not hold exactly the 1 bundles" "$topo_n" "$first_bucket" missing
restore_kv "a bundle whose bytes differ from its recorded SHA-256 is refused" "does not have the SHA-256 the manifest recorded" "$topo_n" "$first_bucket" altered
restore_kv "a manifest without the held rows and the bundles is refused" "a backup older than this check" "$topo_n" "$first_bucket" unrecorded
restore_kv "the topology's bucket count and the bundles get past the check" "no running timescaledb container" "$topo_n" "$first_bucket"

# ---- the Caddy snippet -------------------------------------------------------

# The proof against the pinned Caddy, then against the snippet with the
# /metrics and /readyz refusal removed, which must fail and name them.
if out="$(deploy/caddy/proof.sh 2>&1)"; then
  ok "$(tail -n 1 <<<"$out") (deploy/caddy/proof.sh)"
else
  bad "the Caddy proof:"; grep -E '^FAIL|^proof:' <<<"$out" >&2
fi
perl -0pe 's/\@internal path[^\n]*\n\thandle \@internal \{\n\t\trespond 404\n\t\}\n//' deploy/caddy/ussp.caddy > "$work/no-internal.caddy"
if cmp -s deploy/caddy/ussp.caddy "$work/no-internal.caddy"; then
  bad "the mutation of the snippet changed nothing"
elif out="$(PROOF_SNIPPET="$work/no-internal.caddy" PROOF_PORT=58455 deploy/caddy/proof.sh 2>&1)"; then
  bad "the Caddy proof passes a snippet that routes /metrics and /readyz"
elif grep -q '^FAIL   internal: /metrics' <<<"$out" && grep -q '^FAIL   internal: /readyz' <<<"$out"; then
  ok "the Caddy proof fails a snippet that routes /metrics and /readyz, and names them"
else
  bad "the Caddy proof fails the mutated snippet for another reason:"; grep -E '^FAIL' <<<"$out" >&2
fi

# ---- shellcheck --------------------------------------------------------------

# The pinned v0.10.0 in a container (the same version everywhere), or
# the one installed when Docker cannot run it.
SHELLCHECK_IMAGE="${SHELLCHECK_IMAGE:-koalaman/shellcheck:v0.10.0@sha256:2097951f02e735b613f4a34de20c40f937a6c8f18ecb170612c88c34517221fb}"
mapfile -t scripts < <(git ls-files -co --exclude-standard -- 'deploy/*.sh' 'deploy/**/*.sh' scripts/check-deploy.sh | sort -u)
if MSYS_NO_PATHCONV=1 docker run --rm -v "$repo:/mnt:ro" -w /mnt "$SHELLCHECK_IMAGE" --version >/dev/null 2>&1; then
  if MSYS_NO_PATHCONV=1 docker run --rm -v "$repo:/mnt:ro" -w /mnt "$SHELLCHECK_IMAGE" -x "${scripts[@]}"; then
    ok "shellcheck v0.10.0 (pinned image): ${#scripts[@]} scripts clean"
  else
    bad "shellcheck"
  fi
elif type -P shellcheck >/dev/null 2>&1; then
  if shellcheck -x "${scripts[@]}"; then ok "shellcheck $(shellcheck --version | sed -n 2p): ${#scripts[@]} scripts clean"; else bad "shellcheck"; fi
elif [ -n "${CI:-}" ]; then
  bad "shellcheck could not run (CI must run it)"
else
  echo "check-deploy: SKIPPED shellcheck (neither the pinned image nor a binary runs here)"
fi

echo "check-deploy: $oks checks passed, $fails failed"
[ "$fails" -eq 0 ]
