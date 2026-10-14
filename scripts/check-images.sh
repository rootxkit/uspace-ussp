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
# (LESSONS E-01). The images are run with no network and nothing
# mounted; nothing is started beyond the one --help.
set -euo pipefail

prod="${1:?usage: check-images.sh <production image> <testif image>}"
testif="${2:?usage: check-images.sh <production image> <testif image>}"
command -v docker >/dev/null 2>&1 || { echo "check-images: docker is required" >&2; exit 2; }
# Git Bash would turn /usr/bin/... into a Windows path for docker, and
# the production check would pass for that reason alone.
export MSYS_NO_PATHCONV=1

probe() { # probe <image>: the --help output of ussp-test-interfaces in it, or the failure
  docker run --rm --network none --entrypoint /usr/bin/ussp-test-interfaces "$1" --help 2>&1
}

status=0
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
