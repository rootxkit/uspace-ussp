#!/usr/bin/env bash
# The self-test of scripts/check-images.sh (CI job `image`): stand-in
# images made from the pinned nats-box by docker commit (no build, no
# network for the checks), and check-images.sh run against each pair:
#
#   a production image whose entrypoint runs and has no test interfaces,
#     beside a testif image that answers --help: passes (the presence);
#   a production image whose entrypoint does not run: fails, although
#     the test interfaces are not found in it either (a probe that fails
#     for any reason is not their absence);
#   a production image that contains ussp-test-interfaces: fails;
#   a production image in a registry, not pulled here (as CI's pushed
#     images are): passes, pulled first (a local registry from the
#     pinned registry image, on the loopback).
#
# The images and the registry are removed afterwards, also on failure,
# and their absence checked.
set -euo pipefail
cd "$(dirname "$0")/.."
export MSYS_NO_PATHCONV=1
BASE_IMAGE="${BASE_IMAGE:-natsio/nats-box:0.18.0@sha256:abdc9f9f0120bb8adfbf674eb037d1551db55356eb198b7bd4ffed377f6950a6}"
REGISTRY_IMAGE="${REGISTRY_IMAGE:-registry:2.8.3@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373}"
tag="uspace-ussp-check-images-selftest-$$"
fails=0
pass() { echo "check-images-selftest: ok     $*"; }
fail() { echo "check-images-selftest: FAIL   $*" >&2; fails=$((fails + 1)); }

images=()
reg=""
# shellcheck disable=SC2317 # run by the EXIT trap
cleanup() {
  local rc=$? left
  if [ -n "$reg" ]; then docker rm -f "$reg" >/dev/null 2>&1 || true; fi
  if [ "${#images[@]}" -gt 0 ]; then docker image rm -f "${images[@]}" >/dev/null 2>&1 || true; fi
  left="$(docker image ls -q "$tag" 2>/dev/null)"
  if [ -n "$left" ]; then echo "check-images-selftest: FAIL the stand-in images are still present" >&2; rc=1; fi
  if [ -n "$reg" ] && [ -n "$(docker ps -aq --filter "id=$reg" 2>/dev/null)" ]; then
    echo "check-images-selftest: FAIL the registry container is still present" >&2; rc=1
  fi
  exit "$rc"
}
trap cleanup EXIT

# image <name> <entrypoint> <shell commands>: a stand-in image.
image() {
  local name="$1" ep="$2" cmds="$3" c
  c="$(docker create "$BASE_IMAGE" sh -c "$cmds")"
  docker start -a "$c" >/dev/null
  docker commit --change "ENTRYPOINT [\"$ep\"]" --change 'CMD []' "$c" "$tag:$name" >/dev/null
  docker rm "$c" >/dev/null
  images+=("$tag:$name")
}
script() { # <path> <usage line>: shell commands writing an executable that prints the line
  printf 'printf "#!/bin/sh\necho \\"%s\\"\n" > %s && chmod 0755 %s' "$2" "$1" "$1"
}
image prod /usr/bin/ussp-api "$(script /usr/bin/ussp-api 'usage: ussp-api [--help | healthcheck [path]]')"
image testif /usr/bin/ussp-test-interfaces "$(script /usr/bin/ussp-test-interfaces 'usage: ussp-test-interfaces [--help]')"
image broken /usr/bin/ussp-api 'true'
image leaky /usr/bin/ussp-api "$(script /usr/bin/ussp-api 'usage: ussp-api [--help]') && $(script /usr/bin/ussp-test-interfaces 'usage: ussp-test-interfaces [--help]')"

check() { # <want exit> <label> <production> <testif> [text the output must hold]
  local want="$1" label="$2" rc=0 out
  out="$(scripts/check-images.sh "$tag:$3" "$tag:$4" 2>&1)" || rc=$?
  if [ "$rc" -eq "$want" ] && { [ -z "${5:-}" ] || grep -qF -- "$5" <<<"$out"; }; then
    pass "$label (exit $rc)"
  else
    fail "$label: exit $rc, want $want${5:+ and \"$5\"}"
    while IFS= read -r line; do echo "       $line" >&2; done <<<"$out"
  fi
}
check 0 "a production image whose entrypoint runs, without the test interfaces, passes" prod testif "$tag:prod's entrypoint /usr/bin/ussp-api runs"
check 1 "a production image whose entrypoint does not run fails" broken testif "entrypoint /usr/bin/ussp-api does not run"
check 1 "a production image with ussp-test-interfaces fails" leaky testif "the production image must not contain it"

# The production image in a registry and not here, as the images job
# checks the digests it pushed: docker run pulls it, docker image inspect
# does not, so the check must pull it first. The registry is on the
# daemon's host network, on the loopback, so that the daemon reaches it
# where it runs (Docker Desktop's is not the host's loopback).
regport=$((20000 + RANDOM % 20000))
reg="$(docker run -d --network host -e "REGISTRY_HTTP_ADDR=127.0.0.1:$regport" "$REGISTRY_IMAGE")"
remote="127.0.0.1:$regport/check-images-selftest-$$:prod"
docker tag "$tag:prod" "$remote"
images+=("$remote")
pushed=0
for _ in $(seq 1 30); do
  if docker push -q "$remote" >/dev/null 2>&1; then pushed=1; break; fi
  sleep 1
done
[ "$pushed" -eq 1 ] || { fail "the stand-in production image could not be pushed to the registry"; exit 1; }
docker image rm "$remote" >/dev/null
if docker image inspect "$remote" >/dev/null 2>&1; then
  fail "the pushed production image $remote is still here: the case below would prove nothing"
else
  rc=0
  out="$(scripts/check-images.sh "$remote" "$tag:testif" 2>&1)" || rc=$?
  if [ "$rc" -eq 0 ] && grep -qF "$remote pulled" <<<"$out" && grep -qF "$remote's entrypoint /usr/bin/ussp-api runs" <<<"$out"; then
    pass "a production image in a registry, not pulled here, is pulled and passes (exit 0)"
  else
    fail "a production image in a registry, not pulled here: exit $rc, want 0, pulled"
    while IFS= read -r line; do echo "       $line" >&2; done <<<"$out"
  fi
fi

if [ "$fails" -eq 0 ]; then echo "check-images-selftest: every case passed"; else echo "check-images-selftest: $fails failed" >&2; exit 1; fi
