#!/usr/bin/env bash
# make conformance (docs/WORKPACKAGES/WP-19.md, deploy/conformance/README.md):
# uspace-lab's conformance suite (its WP-L7, cmd/conformance) against this
# USSP, with this repository's conformance target
# (deploy/conformance/target.yaml).
#
#   scripts/conformance.sh [run]   validate, start the stack (or use the
#                                  given target), run the suite, stop the stack
#   scripts/conformance.sh check   validate the configuration only; starts nothing
#   scripts/conformance.sh overrides <out>
#                                  write the contract overrides the run would use
#   scripts/conformance.sh down    remove the stack and check nothing is left
#   scripts/conformance.sh compose <args>
#                                  docker compose on the running stack (the chaos
#                                  procedure, deploy/conformance/chaos.sh)
#
# Two ways to name the USSP under test:
#   - USSP_CONFORMANCE_BASE_URL unset (the default): the stack. This
#     checkout's image (or USSP_CONFORMANCE_IMAGE) runs in the lab's
#     systems stack, the DSS, the lab issuer and the lab Caddy beside it,
#     as compose project CONFORMANCE_PROJECT; stopped and removed after
#     the run unless CONFORMANCE_KEEP=1.
#   - USSP_CONFORMANCE_BASE_URL set: an existing target. The suite's
#     variables come from the environment or from CONFORMANCE_ENV (a
#     KEY=VALUE file, as the lab's targets/README.md lists them), and the
#     caller declares USSP_CONFORMANCE_AUTHORITY_PUSH and
#     USSP_CONFORMANCE_SYSTEM_ID of that target: there is no default to
#     guess them from.
#
# Environment:
#   LAB_DIR                         a uspace-lab checkout (default ../uspace-lab);
#                                   deploy/conformance/SOURCE names the commit read
#   USSP_CONFORMANCE_AUTHORITY_PUSH on|off: USSP_AUTHORITY_PUSH of the target
#                                   (stack default off, the product default)
#   USSP_CONFORMANCE_SYSTEM_ID      USSP_SYSTEM_ID of the target (stack default:
#                                   the lab's, USSP-DEV); must contain TEST or DEV
#   USSP_CONFORMANCE_GROUND_DIR     a directory holding GeographicLib's
#                                   egm2008-2_5.pgm (stack only; default
#                                   local/conformance/ground)
#   USSP_CONFORMANCE_HTTPS_PORT     the lab Caddy on 127.0.0.1 (stack; default 9543)
#   USSP_CONFORMANCE_IMAGE          the image under test (stack; default: built
#                                   from this checkout)
#   CONFORMANCE_PROJECT             compose project (default uspace-ussp-conf)
#   CONFORMANCE_SYSTEMS             ussp (default: the USSP, the DSS, the issuer and
#                                   the lab Caddy) or all (also the authority, the
#                                   CISP and the ANSP of the lab's stack: the chaos
#                                   procedure, docs/RUNBOOKS/chaos.md)
#   CONFORMANCE_KEEP                1 leaves the stack running after the run
#   CONFORMANCE_OUT                 reports (default local/conformance/reports)
#   CONFORMANCE_ALLOW_INCOMPLETE    1 accepts an incomplete run (exit 0, not 3)
#   CONFORMANCE_SUITE_BIN           a built cmd/conformance (default: built from LAB_DIR)
#   GO                              the go command (default go)
#
# Exit status, the suite's: 0 pass; 1 a gate requirement failed (or the
# target's declaration does not match what it answers); 2 a configuration
# error, found before anything is built or started; 3 incomplete (a gate
# requirement was not checked; the report says which and why). A failure
# of the stack itself is 1 and names its step. Secrets stay in files:
# none is printed, put on a command line or written to the reports.
set -euo pipefail
here="$(cd "$(dirname "$0")/.." && pwd)"
cd "$here"

say() {
  echo "conformance: $*"
  if [ -n "${GITHUB_STEP_SUMMARY:-}" ]; then echo "conformance: $*" >>"$GITHUB_STEP_SUMMARY"; fi
}
# usage <message>: a configuration error, exit 2 (nothing was started).
usage() { echo "conformance: $*" >&2; exit 2; }
# die <message>: a failure after something was started, exit 1.
die() { echo "conformance: $*" >&2; exit 1; }
# A path as Docker and Windows programs read it (Git Bash hands them
# /c/...); unchanged elsewhere.
winpath() { if command -v cygpath >/dev/null 2>&1; then cygpath -m "$1"; else printf '%s' "$1"; fi; }
abspath() { case "$1" in /* | [A-Za-z]:/*) printf '%s' "$1" ;; *) printf '%s/%s' "$here" "$1" ;; esac; }

cmd="${1:-run}"
GO="${GO:-go}"
project="${CONFORMANCE_PROJECT:-uspace-ussp-conf}"
state="$here/local/conformance/state"
# The compose command reuses the lab checkout the running stack was
# started from.
if [ -z "${LAB_DIR:-}" ] && [ "$cmd" = compose ] && [ -f "$state/lab-dir" ]; then LAB_DIR="$(cat "$state/lab-dir")"; fi
LAB_DIR="$(abspath "${LAB_DIR:-../uspace-lab}")"
systems="${CONFORMANCE_SYSTEMS:-ussp}"
port="${USSP_CONFORMANCE_HTTPS_PORT:-9543}"
out="$(abspath "${CONFORMANCE_OUT:-local/conformance/reports}")"
external="${USSP_CONFORMANCE_BASE_URL:-}"

# ---- validation (no side effect before it has passed) -------------------------

# The lab's own overrides of the USSP contract, and the declaration
# appended when the push is off.
lab_overrides="$LAB_DIR/conformance/national/contracts/ussp.yaml"
push_off="$here/deploy/conformance/authority-push-off.yaml"

validate() {
  [ -d "$LAB_DIR" ] || usage "LAB_DIR $LAB_DIR is not a directory (a uspace-lab checkout; deploy/conformance/SOURCE names the commit)"
  for f in cmd/conformance/main.go conformance/requirements.yaml conformance/national/contracts/ussp.yaml; do
    [ -f "$LAB_DIR/$f" ] || usage "LAB_DIR $LAB_DIR has no $f: not a uspace-lab checkout with the WP-L7 suite"
  done
  if grep -q '^skip:' "$lab_overrides"; then
    usage "$lab_overrides has its own skip table: merging deploy/conformance/authority-push-off.yaml into it is not defined (read both, then change this script)"
  fi
  if [ -n "${CONFORMANCE_SUITE_BIN:-}" ] && [ ! -x "$CONFORMANCE_SUITE_BIN" ]; then
    usage "CONFORMANCE_SUITE_BIN $CONFORMANCE_SUITE_BIN is not an executable file"
  fi
  case "${CONFORMANCE_ALLOW_INCOMPLETE:-}" in "" | 0 | 1) ;; *) usage "CONFORMANCE_ALLOW_INCOMPLETE is ${CONFORMANCE_ALLOW_INCOMPLETE}: want 1 or unset" ;; esac
  case "${CONFORMANCE_KEEP:-}" in "" | 0 | 1) ;; *) usage "CONFORMANCE_KEEP is ${CONFORMANCE_KEEP}: want 1 or unset" ;; esac
  if [ -n "$external" ]; then
    case "$external" in http://* | https://*) ;; *) usage "USSP_CONFORMANCE_BASE_URL $external is not an http(s) URL" ;; esac
    [ -n "${USSP_CONFORMANCE_AUTHORITY_PUSH:-}" ] || usage "USSP_CONFORMANCE_AUTHORITY_PUSH (on|off) is required with USSP_CONFORMANCE_BASE_URL: the target's USSP_AUTHORITY_PUSH decides what the suite may skip"
    [ -n "${USSP_CONFORMANCE_SYSTEM_ID:-}" ] || usage "USSP_CONFORMANCE_SYSTEM_ID is required with USSP_CONFORMANCE_BASE_URL: the target's USSP_SYSTEM_ID, which must contain TEST or DEV"
    if [ -n "${CONFORMANCE_ENV:-}" ] && [ ! -r "$CONFORMANCE_ENV" ]; then usage "CONFORMANCE_ENV $CONFORMANCE_ENV is not readable"; fi
  else
    for f in deploy/compose.yaml deploy/systems/compose.yaml deploy/systems/gen-secrets.sh deploy/demo.env.example; do
      [ -f "$LAB_DIR/$f" ] || usage "LAB_DIR $LAB_DIR has no $f: the stack needs the lab's systems stack (WP-L6)"
    done
    ground="$(abspath "${USSP_CONFORMANCE_GROUND_DIR:-local/conformance/ground}")"
    [ -s "$ground/egm2008-2_5.pgm" ] || usage "no $ground/egm2008-2_5.pgm: put GeographicLib's egm2008-2_5 grid there (deploy/conformance/README.md, 'Geoid'), or set USSP_CONFORMANCE_GROUND_DIR"
    case "$port" in *[!0-9]* | "") usage "USSP_CONFORMANCE_HTTPS_PORT $port is not a port number" ;; esac
    case "$systems" in ussp | all) ;; *) usage "CONFORMANCE_SYSTEMS is $systems: want ussp or all" ;; esac
    command -v docker >/dev/null 2>&1 || usage "docker is required for the stack"
    command -v curl >/dev/null 2>&1 || usage "curl is required for the stack"
    py="$(command -v python3 || command -v python || true)"
    [ -n "$py" ] || usage "python3 is required for the stack (the issuer's client secrets are split with it)"
    if [ "$systems" = all ]; then
      # The ANSP publishes no image the lab pins by digest: the lab's
      # demo-up.sh builds it once; this profile does not build it.
      local ansp_image
      ansp_image="$(sed -n 's/^ANSP_GO_IMAGE=//p' "$LAB_DIR/deploy/demo.env.example" | tail -n 1)"
      case "$ansp_image" in
        *@sha256:*) ;;
        *) docker image inspect "$ansp_image" >/dev/null 2>&1 || usage "CONFORMANCE_SYSTEMS=all needs the ANSP image $ansp_image, which the lab's deploy/demo-up.sh builds; it is not here" ;;
      esac
    fi
  fi
  push="${USSP_CONFORMANCE_AUTHORITY_PUSH:-off}"
  case "$push" in on | off) ;; *) usage "USSP_CONFORMANCE_AUTHORITY_PUSH is $push: want on or off" ;; esac
  sid="${USSP_CONFORMANCE_SYSTEM_ID:-}"
  if [ -z "$sid" ]; then sid="$(sed -n 's/^USSP_SYSTEM_ID=//p' "$LAB_DIR/deploy/demo.env.example" | tail -n 1)"; fi
  # The safety check of the profile (WP-19 safety notes): a target whose
  # system id is not a test or development one could be a production
  # deployment with production-shaped credentials. Refused, whatever else
  # is configured.
  case "$sid" in
    *TEST* | *DEV*) ;;
    *) usage "USSP_SYSTEM_ID ${sid:-<empty>} contains neither TEST nor DEV: the conformance profile runs against test systems only" ;;
  esac
}

# write_overrides <out>: the lab's overrides of the USSP contract, plus
# the authority-push declaration when the push is off.
write_overrides() {
  local dst="$1"
  mkdir -p "$(dirname "$dst")"
  {
    echo "# Written by uspace-ussp scripts/conformance.sh: $lab_overrides"
    if [ "$push" = off ]; then echo "# plus deploy/conformance/authority-push-off.yaml (USSP_AUTHORITY_PUSH=off)"; fi
    cat "$lab_overrides"
    if [ "$push" = off ]; then
      echo
      cat "$push_off"
    fi
  } >"$dst.tmp"
  mv "$dst.tmp" "$dst"
}

# ---- the stack ------------------------------------------------------------------------

stack_env="$state/stack.env"
dc() {
  docker compose --progress quiet -p "$project" \
    -f "$(winpath "$LAB_DIR/deploy/compose.yaml")" -f "$(winpath "$LAB_DIR/deploy/systems/compose.yaml")" \
    -f "$(winpath "$here/deploy/conformance/compose.yaml")" \
    --env-file "$(winpath "$stack_env")" --env-file "$(winpath "$state/demo/passwords.env")" \
    --profile dss --profile issuer --profile systems "$@"
}

# down: remove the project and check that nothing of it is left (E-02:
# the success of a teardown is checked, not assumed).
down() {
  local log left
  # Its own status is not the verdict (a container already gone makes it
  # complain); what is left afterwards is, and what it said is shown
  # when it failed.
  log="$(docker compose -p "$project" down -v --remove-orphans 2>&1)" || echo "conformance: docker compose down said: $log" >&2
  left="$(docker ps -aq --filter "label=com.docker.compose.project=$project")$(docker volume ls -q --filter "label=com.docker.compose.project=$project")$(docker network ls -q --filter "label=com.docker.compose.project=$project")"
  if [ -n "$left" ]; then
    echo "conformance: compose project $project left containers, volumes or networks behind:" >&2
    docker ps -a --filter "label=com.docker.compose.project=$project" >&2
    docker volume ls --filter "label=com.docker.compose.project=$project" >&2
    docker network ls --filter "label=com.docker.compose.project=$project" >&2
    return 1
  fi
  say "no container, volume or network of $project left"
}

stack_up() {
  image="${USSP_CONFORMANCE_IMAGE:-}"
  if [ -z "$image" ]; then
    local c
    c="$(git rev-parse HEAD)"
    image="uspace-ussp:conformance-${c:0:12}"
    if ! git diff --quiet HEAD -- cmd internal migrations go.mod go.sum deploy/Dockerfile; then
      image="$image-dirty"
    fi
    say "building $image from this checkout"
    docker build -q -f deploy/Dockerfile --build-arg "VERSION=${c:0:7}" --build-arg "COMMIT=$c" -t "$image" . >/dev/null ||
      die "docker build of $image failed"
  fi
  image_id="$(docker image inspect -f '{{.Id}}' "$image")" || die "no image $image"
  say "image under test $image ($image_id)"

  mkdir -p "$state/demo" "$state/issuer/public"
  # The lab's demo.env with this run's values: the state outside the lab
  # checkout, the ground directory, the port, the image. Overridden keys
  # are dropped from the example, not shadowed, so every reader sees one
  # value.
  local keys="DEMO_STATE_DIR|LAB_STATE_DIR|DEMO_GROUND_DIR|DEMO_HTTPS_PORT|USSP_GO_IMAGE"
  if [ -n "${USSP_CONFORMANCE_SYSTEM_ID:-}" ]; then keys="$keys|USSP_SYSTEM_ID"; fi
  {
    grep -Ev "^($keys)=" "$LAB_DIR/deploy/demo.env.example"
    echo "# ---- uspace-ussp scripts/conformance.sh ----"
    echo "DEMO_STATE_DIR=$(winpath "$state/demo")"
    echo "LAB_STATE_DIR=$(winpath "$state/issuer")"
    echo "DEMO_GROUND_DIR=$(winpath "$ground")"
    echo "DEMO_HTTPS_PORT=$port"
    echo "USSP_GO_IMAGE=$image"
    if [ -n "${USSP_CONFORMANCE_SYSTEM_ID:-}" ]; then echo "USSP_SYSTEM_ID=$USSP_CONFORMANCE_SYSTEM_ID"; fi
    echo "USSP_CONFORMANCE_AUTHORITY_PUSH=$push"
  } >"$stack_env"
  val() { sed -n "s/^$1=//p" "$stack_env" | tail -n 1; }

  printf '%s
' "$LAB_DIR" >"$state/lab-dir"
  "$LAB_DIR/deploy/systems/gen-secrets.sh" "$stack_env" >/dev/null || die "the lab's gen-secrets.sh failed"
  LAB_UID="$(id -u)"
  LAB_GID="$(id -g)"
  export LAB_UID LAB_GID MSYS_NO_PATHCONV=1

  local t0
  t0="$(date +%s)"
  dc up -d --build --wait --wait-timeout 600 dss-crdb dss lab-issuer >/dev/null || die "the DSS and the issuer did not become healthy: docker compose -p $project logs dss lab-issuer"
  say "DSS and issuer healthy after $(($(date +%s) - t0)) s"
  [ -s "$state/issuer/client-secrets.json" ] || die "the issuer wrote no client-secrets.json"
  # One file per client, as the systems' *_SECRET_FILE variables read
  # them (the lab's demo-up.sh does the same). Only client ids are printed.
  "$py" - "$(winpath "$state/issuer/client-secrets.json")" "$(winpath "$state/demo/clients")" <<'PY' || die "could not split the issuer's client secrets"
import json, os, sys
src, dst = sys.argv[1], sys.argv[2]
with open(src) as f:
    secrets = json.load(f)
os.makedirs(dst, exist_ok=True)
for client, secret in secrets.items():
    with open(os.path.join(dst, client + ".secret"), "w", newline="\n") as f:
        f.write(secret + "\n")
print("conformance: client secrets written for", ", ".join(sorted(secrets)))
PY
  local svcs
  svcs="$(dc config --services | grep '^ussp-' | tr '\n' ' ')"
  [ -n "$svcs" ] || die "the lab's systems stack names no ussp-* service"
  t0="$(date +%s)"
  # shellcheck disable=SC2086 # one argument per service
  if ! dc up -d --wait --wait-timeout 600 caddy $svcs >/dev/null; then
    # What CI cannot be asked afterwards: the state and the last lines of
    # every USSP service (the processes log their configuration with
    # every password and secret file redacted).
    dc ps -a >&2 || true
    for s in $svcs; do
      echo "conformance: --- last lines of $s ---" >&2
      dc logs --no-color --tail 15 "$s" >&2 || true
    done
    die "the USSP did not become healthy"
  fi
  for m in ussp-migrate ussp-migrate-timeseries; do
    local code
    code="$(docker inspect -f '{{.State.ExitCode}}' "$(dc ps -a -q "$m")")"
    [ "$code" = 0 ] || die "$m exited $code: docker compose -p $project logs $m"
  done
  say "USSP healthy and migrated after $(($(date +%s) - t0)) s"

  ussp_host="$(val USSP_HOST)"
  if [ "$systems" = all ]; then
    # The other three systems, for the chaos procedure
    # (docs/RUNBOOKS/chaos.md): the authority first, because the CISP's
    # api refuses to start until the authority's JWKS answers (the lab's
    # demo-up.sh does the same). curl's own bounded retry waits for it.
    local ah others
    ah="$(val AUTHORITY_HOST)"
    t0="$(date +%s)"
    # shellcheck disable=SC2046 # one argument per service
    dc up -d $(dc config --services | grep '^authority-') >/dev/null || die "the authority did not start"
    curl -sS -o "$(winpath "$state/authority-jwks.json")" --fail --max-time 5 --retry 60 --retry-delay 2 --retry-all-errors \
      --cacert "$(winpath "$state/demo/ca/ca.pem")" --ssl-no-revoke --resolve "$ah:$port:127.0.0.1" "https://$ah:$port/.well-known/jwks.json" ||
      die "the authority's JWKS did not answer through the lab Caddy: docker compose -p $project logs authority-api"
    others="$(dc config --services | grep -E '^(cisp|ansp)-' | tr '\n' ' ')"
    # The CISP's api and the ANSP's each refuse to start until the
    # other's JWKS answers, so one of them restarts once or twice and
    # compose's wait can see it unhealthy on the way. `up` is
    # idempotent: it is asked a second time, and only a second failure
    # stops the run.
    # shellcheck disable=SC2086 # one argument per service
    if ! dc up -d --wait --wait-timeout 600 $others >/dev/null; then
      say "the CISP or the ANSP was not healthy yet (they wait for each other's JWKS): waiting once more"
      # shellcheck disable=SC2086 # one argument per service
      dc up -d --wait --wait-timeout 300 $others >/dev/null || die "the CISP or the ANSP did not become healthy: docker compose -p $project ps"
    fi
    say "authority, CISP and ANSP up after $(($(date +%s) - t0)) s"
  fi
  lab_host="$(val LAB_HOST)"
  ca="$state/demo/ca/ca.pem"
  base="https://$ussp_host:$port"
  {
    echo "USSP_CONFORMANCE_BASE_URL=$base"
    echo "USSP_CONFORMANCE_AUDIENCE=$ussp_host"
    echo "LAB_TOKEN_URL=https://$lab_host:$port/oauth/token"
    echo "LAB_SECRETS_JSON=$(winpath "$state/issuer/client-secrets.json")"
    echo "USSP_CONFORMANCE_CA_FILE=$(winpath "$ca")"
    echo "USSP_CONFORMANCE_RESOLVE_IP=127.0.0.1"
    echo "USSP_IMAGE=$image@$image_id"
  } >"$state/suite.env"
  suite_env="$state/suite.env"
  sign_key="$state/issuer/signing-key.pem"
  curl_tls=(--cacert "$(winpath "$ca")" --ssl-no-revoke --resolve "$ussp_host:$port:127.0.0.1")
}

# check_push_declaration <base url>: what rid-sp answers to an upgrade
# without a credential must match the declared USSP_AUTHORITY_PUSH: off
# is 404 (the path is not served), on is 401 (judged before the
# upgrade). Anything else means the declaration is false, and a run
# with it would skip or fail checks for the wrong reason.
check_push_declaration() {
  local code want
  if [ "$push" = off ]; then want=404; else want=401; fi
  # A GET, bounded, and retried only on a refused connection: the
  # published port can take a moment to answer after Caddy is healthy.
  code="$(curl -sS -o "$(winpath "$state/push-probe.body")" -w '%{http_code}' --max-time 10 \
    --retry 10 --retry-delay 1 --retry-connrefused "${curl_tls[@]}" \
    -H 'Connection: Upgrade' -H 'Upgrade: websocket' -H 'Sec-WebSocket-Version: 13' \
    -H 'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==' "$1/v1/authority/flights")" || code="no answer"
  if [ "$code" != "$want" ]; then
    die "USSP_AUTHORITY_PUSH is declared $push, but GET /v1/authority/flights without a credential answered $code (want $want)"
  fi
  say "USSP_AUTHORITY_PUSH=$push confirmed: /v1/authority/flights without a credential answered $code"
}

# ---- the suite --------------------------------------------------------------------------

suite_bin() {
  if [ -n "${CONFORMANCE_SUITE_BIN:-}" ]; then
    printf '%s' "$CONFORMANCE_SUITE_BIN"
    return 0
  fi
  local dir
  dir="$state/bin"
  mkdir -p "$dir"
  # Built, not `go run`: go run turns every non-zero exit into 1, and the
  # status is the verdict (1 failed, 2 misconfigured, 3 incomplete).
  (cd "$LAB_DIR" && "$GO" build -o "$(winpath "$dir/conformance.exe")" ./cmd/conformance) || die "could not build the lab's cmd/conformance in $LAB_DIR"
  printf '%s' "$dir/conformance.exe"
}

run_suite() {
  local bin rc env_all
  bin="$(suite_bin)"
  env_all="$state/run.env"
  # The suite reads one env file: the target's variables (from the stack
  # or CONFORMANCE_ENV, which may hold tokens: the file is the user's
  # alone) and the paths of this checkout.
  (umask 077 && : >"$env_all")
  {
    if [ -n "${suite_env:-}" ]; then cat "$suite_env"; fi
    if [ -n "${CONFORMANCE_ENV:-}" ]; then cat "$CONFORMANCE_ENV"; fi
    echo "CONFORMANCE_USSP_OPENAPI=$(winpath "$here/api/openapi.yaml")"
    echo "CONFORMANCE_USSP_SCHEMAS=$(winpath "$here/schemas")"
    echo "USSP_CONFORMANCE_OVERRIDES=$(winpath "$state/overrides.yaml")"
  } >"$env_all"
  local args=(run --lab-root "$(winpath "$LAB_DIR")" --target "$(winpath "$here/deploy/conformance/target.yaml")"
    --env-file "$(winpath "$env_all")" --out "$(winpath "$out")")
  if [ -n "${sign_key:-}" ] && [ -s "$sign_key" ]; then args+=(--sign-key "$(winpath "$sign_key")"); fi
  if [ "${CONFORMANCE_ALLOW_INCOMPLETE:-}" = 1 ]; then args+=(--allow-incomplete); fi
  mkdir -p "$out"
  rc=0
  "$bin" "${args[@]}" || rc=$?
  suite_rc="$rc"
}

# ---- main -------------------------------------------------------------------------------

case "$cmd" in
  check)
    validate
    if [ -n "$external" ]; then say "configuration valid: target $external, USSP_AUTHORITY_PUSH=$push, system id $sid"; else say "configuration valid: the stack ($project, systems $systems, port $port), USSP_AUTHORITY_PUSH=$push, system id $sid"; fi
    exit 0
    ;;
  overrides)
    validate
    dst="${2:?usage: scripts/conformance.sh overrides <out>}"
    write_overrides "$dst"
    say "wrote $dst (USSP_AUTHORITY_PUSH=$push)"
    exit 0
    ;;
  down)
    command -v docker >/dev/null 2>&1 || usage "docker is required"
    down
    exit $?
    ;;
  compose)
    # docker compose on the running stack, as this script composes it
    # (deploy/conformance/chaos.sh drives the outages through it).
    [ -f "$stack_env" ] || usage "no stack: $stack_env is missing (CONFORMANCE_KEEP=1 make conformance starts one)"
    shift
    # The issuer runs as the invoking user (it reads its state through a
    # bind mount); without these compose would recreate it as another
    # user that cannot read its key.
    LAB_UID="$(id -u)"
    LAB_GID="$(id -g)"
    export LAB_UID LAB_GID MSYS_NO_PATHCONV=1
    dc "$@"
    exit $?
    ;;
  run) ;;
  *) usage "unknown command $cmd (run, check, overrides, down, compose)" ;;
esac

validate
mkdir -p "$state"
write_overrides "$state/overrides.yaml"
started="$(date +%s)"
suite_env=""
sign_key=""
curl_tls=()
if [ -z "$external" ]; then
  # The stack is removed on every exit, the failing ones included, unless
  # CONFORMANCE_KEEP=1; the teardown's own failure is reported and turns
  # a passing status into 1.
  teardown() {
    local rc=$?
    if [ "${CONFORMANCE_KEEP:-}" = 1 ]; then
      say "CONFORMANCE_KEEP=1: the stack $project is left running (scripts/conformance.sh down removes it)"
      exit "$rc"
    fi
    if ! down; then [ "$rc" -ne 0 ] || rc=1; fi
    exit "$rc"
  }
  trap teardown EXIT
  stack_up
  check_push_declaration "$base"
else
  if [ -n "${USSP_CONFORMANCE_CA_FILE:-}" ]; then curl_tls+=(--cacert "$USSP_CONFORMANCE_CA_FILE" --ssl-no-revoke); fi
  check_push_declaration "$external"
fi
suite_rc=0
run_suite
if [ -z "$external" ]; then
  say "footprint (docker stats, one sample):"
  # shellcheck disable=SC2046 # one argument per container id
  if ! docker stats --no-stream --format 'table {{.Name}}\t{{.MemUsage}}\t{{.CPUPerc}}' $(dc ps -q); then
    say "docker stats failed: no footprint sample for this run"
  fi
fi
# The run's provenance beside its report (E-05): what ran against what.
run_dir="$(find "$out" -mindepth 1 -maxdepth 1 -type d -newermt "@$started" 2>/dev/null | sort | tail -n 1)"
if [ -n "$run_dir" ] && [ -f "$run_dir/report.json" ]; then
  {
    echo "uspace-ussp $(git rev-parse HEAD)$(git diff --quiet HEAD || echo ' (dirty)')"
    echo "uspace-lab $(git -C "$(winpath "$LAB_DIR")" rev-parse HEAD 2>/dev/null || echo unknown)$(git -C "$(winpath "$LAB_DIR")" diff --quiet HEAD 2>/dev/null || echo ' (dirty)')"
    echo "target ${external:-the stack $project ($systems)}"
    echo "image ${image:-not built here}${image_id:+ $image_id}"
    echo "USSP_AUTHORITY_PUSH $push"
    echo "system id $sid"
    echo "suite exit $suite_rc"
  } >"$run_dir/run-info.txt"
  if [ -z "$external" ] && [ -f "$state/issuer/public/jwks.json" ]; then cp "$state/issuer/public/jwks.json" "$run_dir/issuer-jwks.json"; fi
else
  say "no report directory of this run under $out: no run-info.txt written"
fi
case "$suite_rc" in
  0) say "the suite passed after $(($(date +%s) - started)) s; report under $out" ;;
  3) say "the suite's run is incomplete (exit 3): gate requirements were not checked; the report under $out says which and why" ;;
  *) say "the suite exited $suite_rc after $(($(date +%s) - started)) s; report under $out" ;;
esac
exit "$suite_rc"
