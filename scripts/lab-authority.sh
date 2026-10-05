#!/usr/bin/env bash
# The lab clause of H-1: this USSP's occurrence reports delivered to the
# real uspace-authority (test/integration/lab_authority_test.go). Brings
# up test/e2e/authority/compose.yaml (the authority's image: migrate and
# api, on databases and a NATS of their own), runs the test against it,
# and tears the project down, also on failure, then checks that nothing
# of it is left.
#
#   LAB_AUTHORITY_IMAGE=ghcr.io/rootxkit/uspace-authority@sha256:<digest> \
#   LAB_GEOID_DIR=<dir holding egm2008-2_5.pgm> \
#   USSP_TEST_PG_URL=... USSP_TEST_NATS_URL=... scripts/lab-authority.sh
#
# The image is the one built from the commit api/clients/SOURCE pins
# for authority.yaml (its CI publishes it); the run records which.
# USSP_TEST_* are this USSP's own databases and NATS, as `make
# integration` reads them. The keys and the admin password are generated
# into a temporary directory and never printed.
#
# Needs docker compose, openssl, curl and go.
set -euo pipefail
cd "$(dirname "$0")/.."

: "${LAB_AUTHORITY_IMAGE:?the uspace-authority image}"
: "${LAB_GEOID_DIR:?a directory holding egm2008-2_5.pgm}"
[ -s "$LAB_GEOID_DIR/egm2008-2_5.pgm" ] || { echo "lab-authority: $LAB_GEOID_DIR/egm2008-2_5.pgm is missing" >&2; exit 2; }
GO=${GO:-go}
export LAB_AUTHORITY_PROJECT="${LAB_AUTHORITY_PROJECT:-ussp-lab-authority}"
export LAB_AUTHORITY_PORT="${LAB_AUTHORITY_PORT:-58180}"

work="$(mktemp -d)"
if command -v cygpath >/dev/null 2>&1; then
  work="$(cygpath -m "$work")"
  LAB_GEOID_DIR="$(cygpath -m "$LAB_GEOID_DIR")"
fi
export LAB_AUTHORITY_DIR="$work" LAB_GEOID_DIR
umask 077
mkdir -p "$work/keys"
chmod 0755 "$work" "$work/keys"
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:3072 -out "$work/keys/token-1.pem" 2>/dev/null
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:3072 -out "$work/keys/publication-1.pem" 2>/dev/null
for k in pii registry-hash occurrence; do openssl rand -base64 32 > "$work/keys/$k.key"; done
openssl rand -hex 24 > "$work/keys/admin.pw"
# The containers read the keys as their own (nonroot) user.
chmod 0644 "$work/keys/"*
LAB_AUTHORITY_PG_PASSWORD="$(openssl rand -hex 24)"
export LAB_AUTHORITY_PG_PASSWORD

dc() { docker compose -f test/e2e/authority/compose.yaml "$@"; }

# shellcheck disable=SC2317 # run by the EXIT trap
teardown() {
  local rc=$? left
  if [ "$rc" -ne 0 ]; then
    dc logs --no-log-prefix --tail 40 migrate api >&2 2>&1 || true
  fi
  dc down -v --remove-orphans >/dev/null 2>&1 || true
  left="$(docker ps -aq --filter "label=com.docker.compose.project=$LAB_AUTHORITY_PROJECT")"
  left+="$(docker volume ls -q --filter "label=com.docker.compose.project=$LAB_AUTHORITY_PROJECT")"
  left+="$(docker network ls -q --filter "label=com.docker.compose.project=$LAB_AUTHORITY_PROJECT")"
  rm -rf "$work"
  if [ -n "$left" ]; then
    echo "lab-authority: FAIL containers, volumes or networks of $LAB_AUTHORITY_PROJECT are left after teardown" >&2
    rc=1
  else
    echo "lab-authority: teardown verified: nothing of $LAB_AUTHORITY_PROJECT is left"
  fi
  exit "$rc"
}
trap teardown EXIT

echo "lab-authority: image $LAB_AUTHORITY_IMAGE"
dc up -d --wait api
# The image is distroless and has no probe, and /healthz is on the admin
# listener, which is not published: the token service's JWKS on the
# public listener is asked from here, curl retrying a refused connection
# for at most 60 s.
curl -fsS -o /dev/null --retry 30 --retry-connrefused --retry-delay 1 --retry-max-time 60 "http://127.0.0.1:$LAB_AUTHORITY_PORT/.well-known/jwks.json"
echo "lab-authority: api answers GET /.well-known/jwks.json 200 on http://localhost:$LAB_AUTHORITY_PORT (published on 127.0.0.1)"

rc=0
LAB_AUTHORITY_URL="http://localhost:$LAB_AUTHORITY_PORT" LAB_AUTHORITY_ADMIN_PASSWORD_FILE="$work/keys/admin.pw" \
  LAB_AUTHORITY_IMAGE="$LAB_AUTHORITY_IMAGE" \
  "$GO" test -tags "integration lab" -count=1 -p 1 -v -run 'TestLabAuthority' ./test/integration/ 2>&1 | tee lab-authority.log || rc=$?
n=$(grep -c '^--- PASS' lab-authority.log || true)
s=$(grep -c '^--- SKIP' lab-authority.log || true)
echo "lab-authority: go test exited $rc; $n passed, $s skipped"
if [ "$rc" -ne 0 ]; then exit "$rc"; fi
if [ "$n" -eq 0 ] || [ "$s" -ne 0 ]; then echo "lab-authority: the test did not run to a pass" >&2; exit 1; fi
