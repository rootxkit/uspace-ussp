#!/usr/bin/env bash
# shellcheck disable=SC2016 # the jq programs name their own $variables
# Deploys uspace-ussp with deploy/compose/staging.yaml and says what it
# verified (WP-20; LESSONS E-02: a deploy that reports success while
# doing nothing is worse than a failure). The deploy is the owner's act:
# an agent writes this script and never runs it against a server.
#
#   deploy/deploy.sh --env-file <deployment env> [--env-file <more>]... \
#                    [--project uspace-ussp] [-f <extra compose file>]... \
#                    [--verify-only] [--strict] [--accept <service>:<dependency>]... \
#                    [--without-certificate] [--unsigned-local-images]
#
# The env files give compose its variables (deploy/compose/deploy.env.
# example: the images by digest, USSP_HOST, the settings file, the
# directories, the edge network and addresses); the settings file they
# name is added to them for interpolation. In order:
#
#   1. preflight, before anything is pulled or started: the compose file
#      renders; both images are references by digest and every Go service
#      runs the one image; the secrets directory is complete
#      (gen-secrets.sh --check); the settings file sets no secret and an
#      authority-form USSP_SYSTEM_ID; the geoid grid it names is there;
#      the authority's certificate is wired as uspace-deploy's
#      scripts/ussp-certificate.sh writes it (certificate.env with the
#      id; keys/client.secret exactly when the settings name it), or
#      --without-certificate says it is not issued yet;
#   2. verifies both images' cosign signatures and SBOM attestations
#      (deploy/verify-image.sh), then pulls them by those digests;
#   3. starts the database and NATS and waits for their health checks;
#   4. runs both one-shot migrate services, reads each one's exit status
#      and its "migrate done" line, and stops unless both trees are at
#      the latest version this image knows;
#   5. restarts the processes one at a time (tsdb-writer, api,
#      telemetry-ingest, rid-sp, monitor, traffic-ws, dss-sync, then web),
#      each healthy (its /readyz answering) before the next;
#   6. verifies every Go process from inside the project network:
#      /healthz 200, /readyz with every dependency printed, the version its
#      "started" line reports. A required dependency that is not up fails
#      the deploy; an optional one that is not up is printed as DEGRADED
#      and fails it only with --strict. --accept <service>:<dependency>
#      takes one named dependency out of both rules, and says so;
#   7. prints what was verified, or which check failed, and exits non-zero
#      on any failure.
#
# With USSP_EXPECT_VERSION set, a process whose "started" line names
# another version fails 6 (the image's VERSION build argument).
#
# --verify-only does 1 and 6 and changes nothing. --unsigned-local-images
# is the CI smoke's: the images are local builds (no registry, no
# digest), so 2 is skipped and the output says so at every step; a
# reference with a registry host is refused with it.
#
# Every wait is bounded: compose's --wait-timeout, `timeout` around a
# migrate, curl's own timeout and retry count. The probes run from the
# pinned nats-box image on the project's internal network (the Go image
# has no shell and no listener is published). Nothing secret is printed:
# the rendered model holds the secrets and is read with jq only.
set -euo pipefail

NATS_BOX_IMAGE="${NATS_BOX_IMAGE:-natsio/nats-box:0.18.0@sha256:abdc9f9f0120bb8adfbf674eb037d1551db55356eb198b7bd4ffed377f6950a6}"
WAIT_S="${USSP_DEPLOY_WAIT_S:-180}"
here="$(cd "$(dirname "$0")" && pwd)"
compose_file="$here/compose/staging.yaml"
go_services=(tsdb-writer api telemetry-ingest rid-sp monitor traffic-ws dss-sync)
declare -A port=([api]=8080 [telemetry-ingest]=8081 [rid-sp]=8082 [monitor]=8083 [traffic-ws]=8084 [dss-sync]=8085 [tsdb-writer]=8086)

project=uspace-ussp
mode=deploy
strict=0
unsigned=0
without_certificate=0
env_files=()
extra_files=()
accept=()
while [ "$#" -gt 0 ]; do
  case "$1" in
    --env-file) env_files+=(--env-file "${2:?--env-file needs a file}"); shift ;;
    -f) extra_files+=(-f "${2:?-f needs a compose file}"); shift ;;
    --project) project="${2:?--project needs a name}"; shift ;;
    --verify-only) mode=verify-only ;;
    --strict) strict=1 ;;
    --accept) accept+=("${2:?--accept needs <service>:<dependency>}"); shift ;;
    --unsigned-local-images) unsigned=1 ;;
    --without-certificate) without_certificate=1 ;;
    *) echo "deploy: unknown argument $1" >&2; exit 2 ;;
  esac
  shift
done
[ "${#env_files[@]}" -gt 0 ] || { echo "deploy: --env-file is required" >&2; exit 2; }
case "$WAIT_S" in ''|*[!0-9]*) echo "deploy: USSP_DEPLOY_WAIT_S must be whole seconds" >&2; exit 2 ;; esac
for t in docker jq; do type -P "$t" >/dev/null 2>&1 || { echo "deploy: $t not found" >&2; exit 2; }; done

verified=()
log() { echo "deploy: $*"; }
fail() { echo "deploy: FAIL $*" >&2; exit 1; }
jqr() { jq -r "$@" | tr -d '\r'; }

# env_value <name>: the last value the env files give it (compose's
# precedence), quotes removed.
env_value() {
  local f v="" line
  for f in "${env_files[@]}"; do
    [ "$f" = --env-file ] && continue
    [ -r "$f" ] || fail "cannot read $f"
    line="$(grep -E "^$1=" "$f" | tail -n 1 || true)"
    [ -n "$line" ] && v="${line#*=}"
  done
  v="${v%\"}"; v="${v#\"}"
  printf '%s' "$v"
}

settings="$(env_value USSP_SETTINGS_FILE)"
[ -n "$settings" ] || fail "no USSP_SETTINGS_FILE in the env files"
[ -r "$settings" ] || fail "cannot read the settings file $settings"
secrets="$(env_value USSP_SECRETS_DIR)"
[ -n "$secrets" ] || fail "no USSP_SECRETS_DIR in the env files"
dc() { docker compose -p "$project" "${env_files[@]}" --env-file "$settings" -f "$compose_file" "${extra_files[@]}" "$@"; }

# ---- 1. preflight -----------------------------------------------------------

if ! err="$(dc config --quiet 2>&1)"; then
  fail "compose does not render: $err"
fi
rendered="$(dc config --format json)"
image_of() { printf '%s' "$rendered" | jqr --arg s "$1" '.services[$s].image // ""'; }
go_image="$(image_of api)"
web_image="$(image_of web)"
for s in "${go_services[@]}" migrate migrate-timeseries; do
  [ "$(image_of "$s")" = "$go_image" ] || fail "$s runs $(image_of "$s"), not the image api runs ($go_image)"
done
internal_net="$(printf '%s' "$rendered" | jqr '.networks.ussp.name // ""')"
[ -n "$internal_net" ] || fail "the rendered compose has no ussp network"
if [ "$unsigned" -eq 1 ]; then
  for ref in "$go_image" "$web_image"; do
    case "$ref" in */*) fail "--unsigned-local-images with $ref: a registry image is always verified" ;; esac
  done
else
  for ref in "$go_image" "$web_image"; do
    [[ "$ref" =~ ^[a-z0-9./-]+@sha256:[0-9a-f]{64}$ ]] || fail "$ref is not a reference by digest (repo@sha256:<64 hex>)"
  done
fi
log "project $project, images $go_image and $web_image"

if ! "$here/gen-secrets.sh" --check "$secrets" >/dev/null 2>&1; then
  fail "the secrets directory $secrets is incomplete: $("$here/gen-secrets.sh" --check "$secrets" 2>&1 | grep missing | tr '\n' ' ')"
fi
leak="$(grep -oE '^USSP_(PG_URL|TS_URL|NATS_URL|NATS_CREDS|CERTIFICATE_ID|TRUSTED_PROXIES|[A-Z_]*PASSWORD|[A-Z_]*SECRET)=' "$settings" || true)"
[ -z "$leak" ] || fail "the settings file sets $(printf '%s' "$leak" | tr '\n' ' '): secrets and proxies belong to the secrets directory and the compose file"
code="$(printf '%s' "$rendered" | jqr '.services.api.environment.USSP_SYSTEM_ID // ""')"
[[ "$code" =~ ^[A-Z0-9]{1,8}$ ]] || fail "USSP_SYSTEM_ID '$code' is not the authority's form (one to eight upper-case letters and digits)"
geoid="$(printf '%s' "$rendered" | jqr '.services.api.environment.USSP_GEOID_FILE // ""')"
ground="$(env_value USSP_GROUND_DIR)"
if [ -n "$geoid" ]; then
  case "$geoid" in
    /srv/ground/*) [ -s "$ground/${geoid#/srv/ground/}" ] || fail "USSP_GEOID_FILE names $geoid but $ground/${geoid#/srv/ground/} is missing or empty" ;;
    *) fail "USSP_GEOID_FILE $geoid is not under /srv/ground (USSP_GROUND_DIR)" ;;
  esac
fi

# The authority's certificate (Art. 7(6) notices; M8, audit M-6), as
# uspace-deploy's scripts/ussp-certificate.sh writes it from the issue
# answer: certificate.env holds USSP_CERTIFICATE_ID=<32 hex> and nothing
# else, and keys/client.secret the secret of ussp-<code>-01 when the
# client authenticates with a secret. The settings name that file in
# USSP_TOKEN_CLIENT_SECRET_FILE exactly when it is there: a file without
# the line is a secret no process reads (no outgoing call is ever made),
# a line without the file a process that refuses to start.
cert_file="$secrets/certificate.env"
if [ -e "$cert_file" ]; then
  [ -r "$cert_file" ] || fail "cannot read $cert_file"
  if ! grep -qxE 'USSP_CERTIFICATE_ID=[0-9a-f]{32}' "$cert_file" || [ "$(grep -cv '^[[:space:]]*$' "$cert_file")" -ne 1 ]; then
    fail "certificate.env must hold one line USSP_CERTIFICATE_ID=<the authority's 32 hexadecimal characters>, as scripts/ussp-certificate.sh writes it"
  fi
  cert_id="$(sed -n 's/^USSP_CERTIFICATE_ID=//p' "$cert_file")"
  [ "$(printf '%s' "$rendered" | jqr '.services.api.environment.USSP_CERTIFICATE_ID // ""')" = "$cert_id" ] ||
    fail "api does not get USSP_CERTIFICATE_ID from certificate.env"
  log "certificate $cert_id named by the operating-status notices"
elif [ "$without_certificate" -eq 1 ]; then
  log "NO CERTIFICATE: --without-certificate; USSP_CERTIFICATE_ID is unset and no Art. 7(6) notice can be sent (api's /readyz says so)"
  cert_id="none (--without-certificate)"
else
  fail "no certificate.env in $secrets: issue this USSP's certificate at the authority (code $code), then uspace-deploy's scripts/ussp-certificate.sh ANSWER.json; --without-certificate deploys without it"
fi
secret_setting="$(printf '%s' "$rendered" | jqr '.services.api.environment.USSP_TOKEN_CLIENT_SECRET_FILE // ""')"
if [ -n "$secret_setting" ]; then
  case "$secret_setting" in
    /run/secrets/ussp/?*) host_secret="$secrets/keys/${secret_setting#/run/secrets/ussp/}" ;;
    *) fail "USSP_TOKEN_CLIENT_SECRET_FILE $secret_setting is not under /run/secrets/ussp (the secrets directory's keys/)" ;;
  esac
  [ -s "$host_secret" ] || fail "USSP_TOKEN_CLIENT_SECRET_FILE names $secret_setting but ${host_secret#"$secrets"/} is missing or empty (scripts/ussp-certificate.sh writes it)"
  [ "$(wc -l < "$host_secret" | tr -d ' ')" -le 1 ] || fail "${host_secret#"$secrets"/} holds more than one line"
  for s in api rid-sp monitor dss-sync; do
    [ "$(printf '%s' "$rendered" | jqr --arg s "$s" '.services[$s].environment.USSP_TOKEN_CLIENT_SECRET_FILE // ""')" = "$secret_setting" ] ||
      fail "$s does not read the client secret ($s makes outgoing calls)"
  done
  log "outgoing calls as ussp-$code-01 with the secret in ${host_secret#"$secrets"/}"
elif [ -e "$secrets/keys/client.secret" ]; then
  fail "keys/client.secret is there but the settings do not set USSP_TOKEN_CLIENT_SECRET_FILE=/run/secrets/ussp/client.secret: no outgoing call would use it"
else
  log "NO OUTGOING TOKEN CLIENT: USSP_TOKEN_CLIENT_SECRET_FILE is unset (no call to the authority, the CISP, the ANSP or the DSS can be made; each process's /readyz says so)"
fi
verified+=("preflight: renders, images by one reference each, secrets complete, settings hold no secret, USSP_SYSTEM_ID=$code, certificate $cert_id")

# ---- 2. signatures, then pull by digest -------------------------------------

if [ "$mode" = deploy ]; then
  if [ "$unsigned" -eq 1 ]; then
    log "SIGNATURES NOT VERIFIED: --unsigned-local-images (local builds $go_image, $web_image; CI smoke only)"
    verified+=("images: local builds, signatures NOT verified (--unsigned-local-images)")
  else
    "$here/verify-image.sh" "$go_image" "$web_image"
    dc pull --quiet timescaledb nats api web
    verified+=("images: signatures and SBOM attestations verified, pulled by digest")
  fi
fi

# probe <url> [retries]: GET from the pinned tool image on the internal
# network; prints the body, then the status code on its own last line.
probe() {
  MSYS_NO_PATHCONV=1 docker run --rm --network "$internal_net" "$NATS_BOX_IMAGE" \
    curl -sS --max-time 5 --retry "${2:-0}" --retry-delay 1 --retry-all-errors \
    -w '\n%{http_code}' "$1" 2>&1 || true
}

if [ "$mode" = deploy ]; then
  # ---- 3. the database and NATS --------------------------------------------
  dc up -d --wait --wait-timeout "$WAIT_S" timescaledb nats >/dev/null 2>&1 ||
    fail "timescaledb or nats did not become healthy within ${WAIT_S}s: $(dc ps timescaledb nats 2>&1 | tail -n 2 | tr '\n' ' ')"
  log "timescaledb and nats healthy"

  # ---- 4. both trees, and what migrate says --------------------------------
  for m in migrate migrate-timeseries; do
    dc up -d --no-deps --force-recreate "$m" >/dev/null 2>&1 || fail "$m did not start"
    rc=0
    timeout "$WAIT_S" docker compose -p "$project" "${env_files[@]}" --env-file "$settings" -f "$compose_file" "${extra_files[@]}" \
      wait "$m" >/dev/null 2>&1 || rc=$?
    logs="$(dc logs --no-log-prefix "$m" 2>/dev/null | tr -d '\r')"
    [ "$rc" -ne 124 ] || fail "$m did not exit within ${WAIT_S}s"
    [ "$rc" -eq 0 ] || fail "$m exited $rc: $(printf '%s\n' "$logs" | tail -n 3 | tr '\n' ' ')"
    done_line="$(printf '%s\n' "$logs" | jq -cR 'fromjson? | select(.msg == "migrate done")' | tail -n 1)"
    [ -n "$done_line" ] || fail "$m exited 0 without a 'migrate done' line"
    from="$(jqr .from <<<"$done_line")"; to="$(jqr .to <<<"$done_line")"; latest="$(jqr .latest <<<"$done_line")"
    [ "$to" = "$latest" ] || fail "$m left its tree at version $to, the image knows $latest"
    log "$m exited 0: version $from -> $to (latest $latest)"
    verified+=("$m: exit 0, at version $to of $latest")
  done

  # ---- 5. one process at a time, each healthy before the next --------------
  for s in "${go_services[@]}" web; do
    dc up -d --no-deps --wait --wait-timeout "$WAIT_S" "$s" >/dev/null 2>&1 ||
      fail "$s did not become healthy within ${WAIT_S}s: $(dc logs --no-log-prefix --tail 5 "$s" 2>&1 | tr '\n' ' ')"
    log "$s restarted, healthy"
  done
fi

# ---- 6. verify every process ------------------------------------------------

accepted() { # <service> <dependency>
  local a
  for a in "${accept[@]}"; do [ "$a" = "$1:$2" ] && return 0; done
  return 1
}
bad=0
for s in "${go_services[@]}"; do
  out="$(probe "http://$s:${port[$s]}/healthz" 30)"
  if [ "$(printf '%s' "$out" | tail -n 1)" != 200 ]; then
    echo "deploy: FAIL $s: /healthz does not answer 200 ($(printf '%s' "$out" | tail -n 2 | tr '\n' ' '))" >&2
    bad=1
    continue
  fi
  # No retry: /readyz answers 503 with its reasons, and that is the answer.
  out="$(probe "http://$s:${port[$s]}/readyz")"
  code="$(printf '%s' "$out" | tail -n 1)"
  body="$(printf '%s' "$out" | sed '$d')"
  if ! printf '%s' "$body" | jq -e '.status and .dependencies' >/dev/null 2>&1; then
    echo "deploy: FAIL $s: /readyz answered $code without a report: $(printf '%s' "$body" | head -c 200)" >&2
    bad=1
    continue
  fi
  status="$(printf '%s' "$body" | jqr .status)"
  up="$(printf '%s' "$body" | jqr '[.dependencies | to_entries[] | select(.value.state == "up") | .key] | join(",")')"
  note="$status"
  while IFS=$'\t' read -r dep state required detail; do
    [ -n "$dep" ] || continue
    if accepted "$s" "$dep"; then
      log "$s: $dep $state, ACCEPTED by --accept: $detail"
      note+=" ($dep accepted)"
    elif [ "$required" = true ] || [ "$strict" -eq 1 ]; then
      echo "deploy: FAIL $s: /readyz $dep $state$([ "$required" = true ] && echo ' (required)'): $detail" >&2
      bad=1
      note+=" ($dep $state)"
    else
      log "$s: DEGRADED $dep $state: $detail"
      note+=" ($dep $state)"
    fi
  done < <(printf '%s' "$body" | jqr '.dependencies | to_entries[] | select(.value.state != "up") | [.key, .value.state, (.value.required | tostring), (.value.detail // "")] | @tsv')
  if [ "$status" = not_ready ] && [ "$code" = 200 ]; then
    echo "deploy: FAIL $s: /readyz says not_ready with 200" >&2; bad=1
  elif [ "$status" != not_ready ] && [ "$code" != 200 ]; then
    echo "deploy: FAIL $s: /readyz says $status with $code" >&2; bad=1
  fi
  version="$(dc logs --no-log-prefix "$s" 2>/dev/null | tr -d '\r' | jq -rR 'fromjson? | select(.msg == "started") | .version' | tr -d '\r' | tail -n 1)"
  if [ -z "$version" ]; then
    echo "deploy: FAIL $s: no started line in its log" >&2; bad=1
  elif [ -n "${USSP_EXPECT_VERSION:-}" ] && [ "$version" != "$USSP_EXPECT_VERSION" ]; then
    echo "deploy: FAIL $s: runs version $version, expected $USSP_EXPECT_VERSION" >&2; bad=1
  fi
  log "$s: /healthz 200, /readyz $code $note [up: ${up:-none}], version ${version:-?}"
  verified+=("$s: /healthz 200, /readyz $note, version ${version:-?}")
done
if [ "$(dc ps --format '{{.Health}}' web 2>/dev/null | tr -d '\r')" = healthy ]; then
  log "web: health check healthy"
  verified+=("web: health check healthy")
else
  echo "deploy: FAIL web: not healthy ($(dc ps --format '{{.Status}}' web 2>&1 | tr -d '\r'))" >&2
  bad=1
fi

# ---- 7. what was verified ---------------------------------------------------

echo "deploy: verified ($mode, project $project):"
for v in "${verified[@]}"; do echo "deploy:   $v"; done
[ "$bad" -eq 0 ] || fail "some checks failed (above)"
echo "deploy: ok, every check above passed"
