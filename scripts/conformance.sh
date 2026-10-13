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
#                                   the lab's, DEV01); must contain TEST or DEV
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
#   CONFORMANCE_BASELINE            the reviewed baseline the suite gates on
#                                   (default deploy/conformance/baseline.json;
#                                   none: no baseline, every requirement not
#                                   checked makes the run incomplete, exit 3)
#   CONFORMANCE_SUITE_BIN           a built cmd/conformance (default: built from LAB_DIR)
#   CONFORMANCE_QUALIFIER           on (default) | off: the uss_qualifier steps (WP-21).
#                                   On the stack: the test operator seeded, the
#                                   test-interfaces process (uspace-ussp-testif, built
#                                   from this checkout) declared and checked, the lab's
#                                   mock USS beside it and run-qualifier.sh f3411-sp.yaml,
#                                   f3548-scd.yaml and f3411-dp.yaml, each report folded
#                                   into the suite's run; off runs the national suite
#                                   alone and says the InterUSS suites were not run in
#                                   this invocation
#   CONFORMANCE_QUALIFIER_REPORTS   REQ=report.json[,REQ=report.json]: reports of
#                                   uss_qualifier runs made elsewhere, folded in beside
#                                   the stack's (for an external target, the only way)
#   USSP_CONFORMANCE_RID_INJECTION_URL, USSP_CONFORMANCE_RID_OBSERVATION_URL,
#   USSP_CONFORMANCE_FLIGHT_PLANNING_URL
#                                   an external target's declared InterUSS interfaces
#                                   (as uss_qualifier reaches them); each must answer 401
#                                   without a credential before the suite runs
#   USSP_CONFORMANCE_TESTIF_IMAGE   the test-interfaces image (stack; default: built
#                                   from this checkout's deploy/Dockerfile target testif)
#   QUALIFIER_TIMEOUT_S             bound on one uss_qualifier run (the lab's
#                                   run-qualifier.sh; default 3600)
#   GO                              the go command (default go)
#
# Exit status, the suite's: 0 pass; 1 a gate requirement failed (or the
# target's declaration does not match what it answers); 2 a configuration
# error, found before anything is built or started; 3 incomplete (a gate
# requirement was not checked and the baseline does not record it not
# applicable; the report says which and why). A gate requirement the
# baseline records not applicable (F3411-SP and F3548-SCD, blocked by
# the owner decision PLAN §15.2 Q34) does not make the exit 3, and is
# never a pass: the hook names each with the baseline's note, as a
# warning annotation in a workflow, and the report's verdict stays
# incomplete. A failure
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
baseline="${CONFORMANCE_BASELINE:-deploy/conformance/baseline.json}"
if [ "$baseline" != none ]; then baseline="$(abspath "$baseline")"; fi

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
  if [ "$baseline" != none ]; then
    [ -r "$baseline" ] || usage "CONFORMANCE_BASELINE $baseline is not readable (none: run without a baseline)"
    command -v jq >/dev/null 2>&1 || usage "jq is required to read the baseline $baseline"
    jq -e '.format == "conformance-baseline/v1" and (.target | type == "string") and (.requirements | type == "object")' "$baseline" >/dev/null 2>&1 ||
      usage "$baseline is not a conformance-baseline/v1 file with a target and requirements"
    # A requirement accepted not applicable says why, where it is
    # decided: it is printed with every run.
    local unnoted
    unnoted="$(jq -r '.requirements | to_entries[] | select(.value.status == "not_applicable" and ((.value.note // "") == "")) | .key' "$baseline")"
    [ -z "$unnoted" ] || usage "$baseline records not applicable without a note saying why: $(echo "$unnoted" | tr '\n' ' ')"
  fi
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
  qualifier="${CONFORMANCE_QUALIFIER:-on}"
  case "$qualifier" in on | off) ;; *) usage "CONFORMANCE_QUALIFIER is $qualifier: want on or off" ;; esac
  extra_reports=()
  if [ -n "${CONFORMANCE_QUALIFIER_REPORTS:-}" ]; then
    local pair pairs
    IFS=',' read -r -a pairs <<<"$CONFORMANCE_QUALIFIER_REPORTS"
    for pair in "${pairs[@]}"; do
      case "$pair" in
        ?*=?*) [ -r "${pair#*=}" ] || usage "CONFORMANCE_QUALIFIER_REPORTS: ${pair#*=} is not readable" ;;
        *) usage "CONFORMANCE_QUALIFIER_REPORTS: $pair is not REQ=report.json" ;;
      esac
      extra_reports+=("$pair")
    done
  fi
  if [ -z "$external" ] && [ "$qualifier" = on ]; then
    for f in conformance/uss_qualifier/run-qualifier.sh conformance/uss_qualifier/compose.yaml conformance/uss_qualifier/f3411-sp.yaml \
      conformance/uss_qualifier/f3548-scd.yaml conformance/uss_qualifier/f3411-dp.yaml deploy/systems/Caddyfile \
      conformance/fixtures/test-operators.yaml cmd/demo-seed/main.go sim/sitl.env.example; do
      [ -f "$LAB_DIR/$f" ] || usage "LAB_DIR $LAB_DIR has no $f: the uss_qualifier steps need the lab's WP-L7 configurations (CONFORMANCE_QUALIFIER=off skips them)"
    done
    command -v timeout >/dev/null 2>&1 || usage "timeout is required for the uss_qualifier steps (CONFORMANCE_QUALIFIER=off skips them)"
    command -v jq >/dev/null 2>&1 || usage "jq is required for the uss_qualifier steps (CONFORMANCE_QUALIFIER=off skips them)"
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
  # The test-interfaces image (D13): a separate target of the same
  # Dockerfile, never part of the image under test.
  testif_image="${USSP_CONFORMANCE_TESTIF_IMAGE:-}"
  if [ -z "$testif_image" ]; then
    local tc
    tc="$(git rev-parse HEAD)"
    testif_image="uspace-ussp-testif:conformance-${tc:0:12}"
    if ! git diff --quiet HEAD -- cmd internal migrations go.mod go.sum deploy/Dockerfile; then testif_image="$testif_image-dirty"; fi
    docker build -q -f deploy/Dockerfile --target testif --build-arg "VERSION=${tc:0:7}" --build-arg "COMMIT=$tc" -t "$testif_image" . >/dev/null ||
      die "docker build of $testif_image failed"
  fi
  testif_image_id="$(docker image inspect -f '{{.Id}}' "$testif_image")" || die "no image $testif_image"
  # The image under test has no ussp-test-interfaces, the testif image
  # has (D13; the same check as CI's image jobs).
  scripts/check-images.sh "$image" "$testif_image" >"$state/check-images.log" 2>&1 ||
    { cat "$state/check-images.log" >&2; die "the image check failed (above)"; }
  say "test-interfaces image $testif_image ($testif_image_id); the image under test has no ussp-test-interfaces"

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
    echo "USSP_TESTIF_IMAGE=$testif_image"
    echo "USSP_CONFORMANCE_CADDYFILE=$(winpath "$state/Caddyfile")"
    echo "USSP_CONFORMANCE_CA_PEM=$(winpath "$state/demo/ca/ca.pem")"
    echo "USSP_TESTIF_STATE_DIR=$(winpath "$testif_state")"
    echo "USSP_TESTIF_OPERATOR_FILE=$(winpath "$here/deploy/conformance/test-operator.yaml")"
  } >"$stack_env"
  mkdir -p "$testif_state"
  write_caddyfile "$state/Caddyfile"
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
  # The authority before the test operator's seed when the qualifier
  # runs: the USSP admits the operator, and creates its client, only when
  # the authority's registry answers F8 validity `valid`, so the lab's
  # registry seed of the operator (uspace-lab
  # conformance/fixtures/test-operators.yaml) runs at the authority
  # first. The chaos procedure (systems all) starts it here too: the
  # CISP's api refuses to start until the authority's JWKS answers.
  if [ "$systems" = all ] || [ "$qualifier" = on ]; then
    authority_up
  fi
  if [ "$qualifier" = on ]; then
    registry_seed
  fi
  testif_up

  ussp_host="$(val USSP_HOST)"
  if [ "$systems" = all ]; then
    # The other two systems, for the chaos procedure
    # (docs/RUNBOOKS/chaos.md).
    local others
    t0="$(date +%s)"
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
    say "CISP and ANSP up after $(($(date +%s) - t0)) s"
  fi
  lab_host="$(val LAB_HOST)"
  # The interfaces as uss_qualifier reaches them on the lab network
  # (the alias ussp of test-interfaces): the target's `interfaces`.
  participant="ussp-$sid-01"
  iface_injection=http://ussp:8087/test/rid/v1/injection
  iface_observation=http://ussp:8087/test/rid/v1/observation
  iface_planning=http://ussp:8087/test/flight_planning/v1
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
    echo "USSP_CONFORMANCE_PARTICIPANT=$participant"
    echo "USSP_CONFORMANCE_RID_INJECTION_URL=$iface_injection"
    echo "USSP_CONFORMANCE_RID_OBSERVATION_URL=$iface_observation"
    echo "USSP_CONFORMANCE_FLIGHT_PLANNING_URL=$iface_planning"
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

# ---- the test interfaces (WP-21) --------------------------------------------------------

testif_state="$state/testif"

# write_caddyfile <out>: the lab Caddy's Caddyfile with /test/* on the
# USSP host routed to the test-interfaces process, before every other
# handle of that host. The lab's file is not changed; a Caddyfile
# without the USSP host's block is refused rather than run without the
# route.
write_caddyfile() {
  local src="$LAB_DIR/deploy/systems/Caddyfile" dst="$1"
  [ -f "$src" ] || die "the lab has no deploy/systems/Caddyfile"
  awk '
    { print }
    /^\{\$USSP_HOST\} \{$/ { inblock = 1; next }
    inblock && /^\timport no_metrics$/ {
      print "\t# uspace-ussp scripts/conformance.sh (WP-21): the InterUSS test interfaces"
      print "\t@testif path /test/*"
      print "\thandle @testif {"
      print "\t\treverse_proxy ussp-test-interfaces:8087"
      print "\t}"
      inblock = 0; done = 1
    }
    END { if (!done) exit 3 }
  ' "$src" >"$dst.tmp" || die "the lab's Caddyfile has no {\$USSP_HOST} block with 'import no_metrics': the /test/* route cannot be added"
  mv "$dst.tmp" "$dst"
}

# authority_up: the lab's authority, waited for through the lab Caddy
# (curl's own bounded retry; its JWKS answers once the api is up).
authority_up() {
  local ah t0
  ah="$(val AUTHORITY_HOST)"
  t0="$(date +%s)"
  # shellcheck disable=SC2046 # one argument per service
  dc up -d $(dc config --services | grep '^authority-') >/dev/null || die "the authority did not start"
  curl -sS -o "$(winpath "$state/authority-jwks.json")" --fail --max-time 5 --retry 60 --retry-delay 2 --retry-all-errors \
    --cacert "$(winpath "$state/demo/ca/ca.pem")" --ssl-no-revoke --resolve "$ah:$port:127.0.0.1" "https://$ah:$port/.well-known/jwks.json" ||
    die "the authority's JWKS did not answer through the lab Caddy: docker compose -p $project logs authority-api"
  say "authority up after $(($(date +%s) - t0)) s"
}

# registry_seed: the lab's registry step (cmd/demo-seed --steps registry)
# at the authority, through its console API: the lab's operators, the
# scenarios' aircraft and the test operators of
# conformance/fixtures/test-operators.yaml with their serials, before
# the USSP is asked about them. Built, not `go run`, so its status is
# its own; its log is kept in the state directory (it names no secret).
registry_seed() {
  local dir t0
  dir="$state/bin"
  mkdir -p "$dir"
  (cd "$LAB_DIR" && "$GO" build -o "$(winpath "$dir/demo-seed.exe")" ./cmd/demo-seed) || die "could not build the lab's cmd/demo-seed in $LAB_DIR"
  t0="$(date +%s)"
  (cd "$LAB_DIR" && "$dir/demo-seed.exe" --env "$(winpath "$stack_env")" --lab sim/sitl.env.example --steps registry \
    --test-operators conformance/fixtures/test-operators.yaml scenarios/*.yaml) >"$state/registry-seed.log" 2>&1 ||
    { tail -n 20 "$state/registry-seed.log" >&2; die "the lab's registry seed failed (above; $state/registry-seed.log)"; }
  say "lab registry seeded at the authority after $(($(date +%s) - t0)) s"
}

# testif_up: the test operator seeded through the national API (a staff
# user by `ussp-api staff-add` for the console session), then the
# test-interfaces process. Passwords are generated into the state
# directory; none is printed or put on a command line.
testif_up() {
  mkdir -p "$testif_state"
  chmod 700 "$testif_state" 2>/dev/null || true
  local staff=wp21.staff.support log
  if [ ! -s "$testif_state/staff-password" ]; then
    (umask 077 && "$py" -c 'import secrets; print("Wp21-" + secrets.token_urlsafe(24))' >"$testif_state/staff-password") ||
      die "could not write the staff password"
  fi
  # staff-add reads the password on stdin; a second run on the same
  # stack finds the user, which is not a failure here.
  if ! log="$(dc exec -T ussp-api /usr/bin/ussp-api staff-add "$staff" support <"$testif_state/staff-password" 2>&1)"; then
    case "$log" in
      *exists*) say "staff user $staff exists" ;;
      *) echo "conformance: ussp-api staff-add said: $log" >&2; die "could not add the staff user for the console session" ;;
    esac
  fi
  dc run --rm --no-deps -T -v "$(winpath "$testif_state"):/state" ussp-test-interfaces \
    seed --state-dir /state --console-user "$staff" --console-password-file /state/staff-password >"$state/seed.log" 2>&1 ||
    { tail -n 20 "$state/seed.log" >&2; die "the test operator's seed failed (above; $state/seed.log)"; }
  [ -s "$testif_state/seed.json" ] || die "the seed wrote no seed.json"
  USSP_TESTIF_OPERATOR_CLIENT_ID="$(tr -d '\r\n' <"$testif_state/operator-client-id")"
  export USSP_TESTIF_OPERATOR_CLIENT_ID
  say "test operator: $(jq -c '{operator_status, validation_status, client_id, bound: (.bound | length), console_session}' "$(winpath "$testif_state/seed.json")")"
  jq -r '.notes[]' "$(winpath "$testif_state/seed.json")" | while IFS= read -r n; do say "test operator: $n"; done
  dc up -d --wait --wait-timeout 120 ussp-test-interfaces >/dev/null ||
    { dc logs --no-color --tail 20 ussp-test-interfaces >&2 || true; die "test-interfaces did not become healthy"; }
  say "test-interfaces healthy ($testif_image)"
}

# check_interfaces <rid_injection url> <rid_observation url> <flight_planning url>:
# every declared interface must answer a call without a credential 401
# (as WP-19's push check: a target cannot declare what it does not
# serve). An empty URL is not declared and not called.
check_interfaces() {
  local name url path code pair
  for pair in "rid_injection|$1|/user_notifications?after=2026-01-01T00:00:00Z" "rid_observation|$2|/display_data?view=41.71,44.82,41.72,44.83" "flight_planning|$3|/status"; do
    IFS='|' read -r name url path <<<"$pair"
    [ -n "$url" ] || continue
    code="$(curl -sS -o "$(winpath "$state/iface-probe.body")" -w '%{http_code}' --max-time 10 --retry 10 --retry-delay 1 --retry-connrefused \
      "${curl_tls[@]}" "$url$path")" || code="no answer"
    [ "$code" = 401 ] || die "the target declares $name at $url, but $path without a credential answered $code (want 401)"
    say "interface $name declared and served: $path without a credential answered 401"
  done
}

# run_qualifier: the lab's mock USS beside the stack and its
# run-qualifier.sh for each configuration the target's interfaces allow;
# every report is folded into the suite's run. A run that writes no
# report fails the hook (after the suite), never passes as not run.
qreports=()
qualifier_failed=0
run_qualifier() {
  local qdir secret req cfg pair t0 rc
  qdir="$out/qualifier-$(date -u +%Y%m%dT%H%M%SZ)"
  mkdir -p "$qdir"
  secret="$(jq -r '."lab-01" // empty' "$(winpath "$state/issuer/client-secrets.json")")"
  [ -n "$secret" ] || die "the issuer's client-secrets.json has no lab-01"
  MOCK_USS_AUTH_SPEC="ClientIdClientSecret(token_endpoint=http://lab-issuer:8080/oauth/token,client_id=lab-01,client_secret=${secret},send_request_as_data=true)"
  export MOCK_USS_AUTH_SPEC
  QUALIFIER_DOCKER_NETWORK="${project}_lab"
  LAB_STATE_DIR="$(winpath "$state/issuer")"
  QUALIFIER_PARTICIPANT="$participant"
  QUALIFIER_TARGET_URL_REGEX="https://${ussp_host//./\\.}(:[0-9]+)?/.*"
  QUALIFIER_DSS_URL=http://dss:8082
  QUALIFIER_DSS_HOST=dss
  QUALIFIER_MOCK_RIDSP_URL=http://mock-ridsp
  QUALIFIER_MOCK_RIDDP_URL=http://mock-riddp
  QUALIFIER_MOCK_SCD_URL=http://mock-scd
  QUALIFIER_MOCK_URL_REGEX='http://mock-.*'
  QUALIFIER_TOKEN_URL=http://lab-issuer:8080/oauth/token
  LAB_SECRETS_JSON="$state/issuer/client-secrets.json"
  # The lab CA certificate (never its key): uss_qualifier and the mock
  # USS query this USSP's /uss/flights over the lab's TLS.
  QUALIFIER_CA_FILE="$(winpath "$state/demo/ca/ca.pem")"
  export QUALIFIER_DOCKER_NETWORK LAB_STATE_DIR QUALIFIER_PARTICIPANT QUALIFIER_TARGET_URL_REGEX QUALIFIER_DSS_URL QUALIFIER_DSS_HOST \
    QUALIFIER_MOCK_RIDSP_URL QUALIFIER_MOCK_RIDDP_URL QUALIFIER_MOCK_SCD_URL QUALIFIER_MOCK_URL_REGEX QUALIFIER_TOKEN_URL LAB_SECRETS_JSON QUALIFIER_CA_FILE
  docker compose --progress quiet -p "$project-qualifier" -f "$(winpath "$LAB_DIR/conformance/uss_qualifier/compose.yaml")" up -d >/dev/null ||
    die "the lab's mock USS did not start: docker compose -p $project-qualifier logs"
  unset MOCK_USS_AUTH_SPEC
  say "mock USS up ($project-qualifier on $QUALIFIER_DOCKER_NETWORK)"
  for pair in "F3411-SP|f3411-sp.yaml|$iface_injection" "F3548-SCD|f3548-scd.yaml|$iface_planning" "F3411-DP|f3411-dp.yaml|$iface_observation"; do
    IFS='|' read -r req cfg QUALIFIER_TARGET_INTERFACE_URL <<<"$pair"
    export QUALIFIER_TARGET_INTERFACE_URL
    if [ -z "$QUALIFIER_TARGET_INTERFACE_URL" ]; then
      say "uss_qualifier $cfg: the target declares no interface for $req; not run"
      continue
    fi
    rc=0
    t0="$(date +%s)"
    "$LAB_DIR/conformance/uss_qualifier/run-qualifier.sh" "$cfg" "$qdir/$req" >"$qdir/$req.log" 2>&1 || rc=$?
    if [ "$rc" -ne 0 ] || [ ! -s "$qdir/$req/qualifier/report.json" ]; then
      tail -n 20 "$qdir/$req.log" >&2
      say "uss_qualifier $cfg wrote no report (exit $rc, $(($(date +%s) - t0)) s; $qdir/$req.log): $req is not judged by this run"
      qualifier_failed=1
      continue
    fi
    say "uss_qualifier $cfg finished after $(($(date +%s) - t0)) s: $(grep 'uss_qualifier exited' "$qdir/$req.log" | tail -n 1)"
    qreports+=("$req=$qdir/$req/qualifier/report.json")
  done
}

# qualifier_down: remove the mock USS project and check nothing is left.
qualifier_down() {
  local log
  [ "${qualifier:-off}" = on ] && [ -z "$external" ] || return 0
  log="$(docker compose -p "$project-qualifier" down -v --remove-orphans 2>&1)" || echo "conformance: docker compose down ($project-qualifier) said: $log" >&2
  if [ -n "$(docker ps -aq --filter "label=com.docker.compose.project=$project-qualifier")" ]; then
    echo "conformance: the mock USS project $project-qualifier left containers behind" >&2
    docker ps -a --filter "label=com.docker.compose.project=$project-qualifier" >&2
    return 1
  fi
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
  if [ "$baseline" != none ]; then args+=(--baseline "$(winpath "$baseline")"); fi
  local r
  for r in "${qreports[@]}" "${extra_reports[@]}"; do
    args+=(--qualifier-report "${r%%=*}=$(winpath "${r#*=}")")
  done
  mkdir -p "$out"
  rc=0
  "$bin" "${args[@]}" || rc=$?
  suite_rc="$rc"
}

# not_checked <report.json>: the gate requirements the run did not check,
# one "<id><TAB><the baseline's note>" per line (note empty when the
# baseline does not record it, or there is none).
not_checked() {
  local bl='{}'
  if [ "$baseline" != none ]; then bl="$(cat "$baseline")"; fi
  jq -r --argjson bl "$bl" '.requirements[]? | select(.role == "gate" and .status == "not_applicable")
    | [.id, (($bl.requirements // {})[.id] | select(.status == "not_applicable") | .note) // ""] | @tsv' "$1"
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
    if ! qualifier_down; then [ "$rc" -ne 0 ] || rc=1; fi
    if ! down; then [ "$rc" -ne 0 ] || rc=1; fi
    exit "$rc"
  }
  trap teardown EXIT
  stack_up
  check_push_declaration "$base"
  # Declared as uss_qualifier reaches them (the lab network); checked as
  # the host reaches them, through the lab Caddy's /test/* route.
  check_interfaces "$base/test/rid/v1/injection" "$base/test/rid/v1/observation" "$base/test/flight_planning/v1"
  if [ "$qualifier" = on ]; then
    run_qualifier
  else
    say "CONFORMANCE_QUALIFIER=off: uss_qualifier (F3411-SP, F3548-SCD, F3411-DP) not run in this invocation"
  fi
else
  if [ -n "${USSP_CONFORMANCE_CA_FILE:-}" ]; then curl_tls+=(--cacert "$USSP_CONFORMANCE_CA_FILE" --ssl-no-revoke); fi
  check_push_declaration "$external"
  check_interfaces "${USSP_CONFORMANCE_RID_INJECTION_URL:-}" "${USSP_CONFORMANCE_RID_OBSERVATION_URL:-}" "${USSP_CONFORMANCE_FLIGHT_PLANNING_URL:-}"
  if [ "${#extra_reports[@]}" -eq 0 ]; then
    say "external target: this hook runs no uss_qualifier against it (CONFORMANCE_QUALIFIER_REPORTS folds reports made elsewhere): not run in this invocation"
  fi
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
run_dir="$(find "$out" -mindepth 1 -maxdepth 1 -type d ! -name 'qualifier-*' -newermt "@$started" 2>/dev/null | sort | tail -n 1)"
if [ -n "$run_dir" ] && [ -f "$run_dir/report.json" ]; then
  {
    echo "uspace-ussp $(git rev-parse HEAD)$(git diff --quiet HEAD || echo ' (dirty)')"
    echo "uspace-lab $(git -C "$(winpath "$LAB_DIR")" rev-parse HEAD 2>/dev/null || echo unknown)$(git -C "$(winpath "$LAB_DIR")" diff --quiet HEAD 2>/dev/null || echo ' (dirty)')"
    echo "target ${external:-the stack $project ($systems)}"
    echo "image ${image:-not built here}${image_id:+ $image_id}"
    echo "USSP_AUTHORITY_PUSH $push"
    echo "system id $sid"
    echo "suite exit $suite_rc"
    echo "baseline ${baseline#"$here"/}"
    echo "test-interfaces image ${testif_image:-not built here}${testif_image_id:+ $testif_image_id}"
    echo "uss_qualifier ${qualifier:-on}: reports ${qreports[*]:-none}${extra_reports[*]:+ ${extra_reports[*]}}"
    if [ "$qualifier_failed" = 1 ]; then echo "uss_qualifier: a configuration wrote no report (the hook exits 1)"; fi
  } >"$run_dir/run-info.txt"
  if [ -z "$external" ] && [ -f "$state/issuer/public/jwks.json" ]; then cp "$state/issuer/public/jwks.json" "$run_dir/issuer-jwks.json"; fi
else
  say "no report directory of this run under $out: no run-info.txt written"
fi
# A gate requirement not checked is never a pass, even when the
# baseline accepts it: each is named with the reason it is not checked.
blocked=0
if [ -n "$run_dir" ] && [ -f "$run_dir/report.json" ] && [ "$baseline" != none ]; then
  if ! nc="$(not_checked "$(winpath "$run_dir/report.json")")"; then
    say "could not read the requirements of $run_dir/report.json"
    [ "$suite_rc" -ne 0 ] || suite_rc=1
    nc=""
  fi
  while IFS=$'\t' read -r id note; do
    [ -n "$id" ] || continue
    if [ -n "$note" ]; then
      blocked=$((blocked + 1))
      say "NOT CHECKED, NOT A PASS: $id: $note (accepted not applicable by ${baseline#"$here"/})"
      if [ "${GITHUB_ACTIONS:-}" = true ]; then echo "::warning title=conformance: $id not checked (not a pass)::$note"; fi
      echo "not checked $id: $note" >>"$run_dir/run-info.txt"
    else
      say "NOT CHECKED: $id (no baseline records it not applicable: the run is incomplete)"
    fi
  done <<<"$nc"
fi
if [ "$qualifier_failed" = 1 ] && [ "$suite_rc" -ne 1 ]; then
  say "a uss_qualifier configuration wrote no report: its requirement was not judged; exit 1, whatever the suite said ($suite_rc)"
  suite_rc=1
fi
case "$suite_rc" in
  0)
    if [ "$blocked" -gt 0 ]; then
      say "the suite exited 0 after $(($(date +%s) - started)) s with $blocked gate requirements not checked, each accepted by the reviewed baseline: the verdict is incomplete, not a pass; report under $out"
    elif [ "${CONFORMANCE_ALLOW_INCOMPLETE:-}" = 1 ]; then
      say "the suite exited 0 after $(($(date +%s) - started)) s: a pass, or an incomplete run accepted by CONFORMANCE_ALLOW_INCOMPLETE=1 (the report says which); report under $out"
    else
      say "the suite passed after $(($(date +%s) - started)) s; report under $out"
    fi
    ;;
  3) say "the suite's run is incomplete (exit 3): gate requirements were not checked; the report under $out says which and why" ;;
  *) say "the suite exited $suite_rc after $(($(date +%s) - started)) s; report under $out" ;;
esac
exit "$suite_rc"
