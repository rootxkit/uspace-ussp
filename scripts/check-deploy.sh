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
  printf '  %s\n' "$findings"
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
