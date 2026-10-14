#!/usr/bin/env bash
# The production image never contains the InterUSS test interfaces
# (docs/PLAN.md D13; brief WP-21), and the testif image does:
#
#   scripts/check-images.sh <production image> <testif image>
#
# Each image is asked to run /usr/bin/ussp-test-interfaces --help: the
# production image must fail to (there is no such file), the testif image
# must answer with the process's usage. Both are checked, so that a check
# that could never find the binary is not mistaken for its absence
# (LESSONS E-01), and the production image's own entrypoint must answer
# its --help, so that an image that cannot run at all is not either. The
# images are run with no network and nothing mounted; nothing is started
# beyond each --help. scripts/check-images-selftest.sh tests this script.
set -euo pipefail

prod="${1:?usage: check-images.sh <production image> <testif image>}"
testif="${2:?usage: check-images.sh <production image> <testif image>}"
command -v docker >/dev/null 2>&1 || { echo "check-images: docker is required" >&2; exit 2; }
# Git Bash would turn /usr/bin/... into a Windows path for docker, and
# the production check would pass for that reason alone.
export MSYS_NO_PATHCONV=1

# The images job checks what it pushed, by digest: docker run would pull
# it, docker image inspect does not, and an image that is not here is
# not an image that does not run. Each is pulled when it is not here.
for img in "$prod" "$testif"; do
  if ! docker image inspect "$img" >/dev/null 2>&1; then
    docker pull -q "$img" >/dev/null || { echo "check-images: $img is not here and cannot be pulled" >&2; exit 1; }
    echo "check-images: $img pulled"
  fi
done

probe() { # probe <image>: the --help output of ussp-test-interfaces in it, or the failure
  docker run --rm --network none --entrypoint /usr/bin/ussp-test-interfaces "$1" --help 2>&1
}

status=0
# The production image's own entrypoint must run (its --help answers
# with its usage): a probe of an image that cannot run at all fails too,
# and would pass below as the absence of the test interfaces.
out=""
if ep_path="$(docker image inspect -f '{{index .Config.Entrypoint 0}}' "$prod" 2>&1)" &&
  out="$(docker run --rm --network none "$prod" --help 2>&1)" &&
  first="${out%%$'\n'*}" && { [ "$first" = "usage: ${ep_path##*/}" ] || [[ "$first" == "usage: ${ep_path##*/} "* ]]; }; then
  echo "check-images: $prod's entrypoint $ep_path runs (--help answers with its usage)"
else
  echo "check-images: $prod's entrypoint ${ep_path:-(none)} does not run: the absence below would prove nothing" >&2
  printf '%s\n' "$out" | head -n 3 >&2
  status=1
fi
if out="$(probe "$prod")"; then
  echo "check-images: $prod runs ussp-test-interfaces: the production image must not contain it" >&2
  echo "$out" | head -n 3 >&2
  status=1
else
  echo "check-images: $prod has no ussp-test-interfaces"
fi
if out="$(probe "$testif")" && echo "$out" | grep -q '^usage: ussp-test-interfaces'; then
  echo "check-images: $testif runs ussp-test-interfaces --help"
else
  echo "check-images: $testif does not run ussp-test-interfaces --help: the check above proves nothing" >&2
  echo "$out" | head -n 3 >&2
  status=1
fi
# The production entrypoint is not test-interfaces either.
ep="$(docker image inspect -f '{{json .Config.Entrypoint}}' "$prod")"
case "$ep" in
  *test-interfaces*) echo "check-images: $prod's entrypoint is $ep" >&2; status=1 ;;
  *) echo "check-images: $prod's entrypoint is $ep" ;;
esac
exit "$status"
