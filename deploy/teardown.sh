#!/usr/bin/env bash
# Removes a uspace-ussp compose project and proves it is gone (E-02: a
# teardown that reports success must have verified it).
#
#   deploy/teardown.sh [--project uspace-ussp] [--volumes] [--network <name>]...
#
# Stops and removes every container of the project (by its compose
# label, so it needs no compose file or env), with --volumes also its
# named volumes (the databases, the JetStream store and the record
# bundles: data is lost; take a backup first, docs/RUNBOOKS/backup.md),
# and each --network named (the smoke's edge network; never the shared
# edge network of a server). Then it lists what is left of the project
# and exits 0 only when nothing is: without --volumes the volumes are
# expected to stay and are listed as kept.
set -euo pipefail

project=uspace-ussp
volumes=0
networks=()
while [ "$#" -gt 0 ]; do
  case "$1" in
    --project) project="${2:?--project needs a name}"; shift ;;
    --volumes) volumes=1 ;;
    --network) networks+=("${2:?--network needs a name}"); shift ;;
    *) echo "teardown: unknown argument $1" >&2; exit 2 ;;
  esac
  shift
done
label="label=com.docker.compose.project=$project"

ids="$(docker ps -aq --filter "$label")"
if [ -n "$ids" ]; then
  # shellcheck disable=SC2086 # one id per word
  docker rm -f $ids >/dev/null
fi
if [ "$volumes" -eq 1 ]; then
  vols="$(docker volume ls -q --filter "$label")"
  # shellcheck disable=SC2086
  [ -z "$vols" ] || docker volume rm $vols >/dev/null
fi
nets="$(docker network ls -q --filter "$label")"
# shellcheck disable=SC2086
[ -z "$nets" ] || docker network rm $nets >/dev/null
for n in "${networks[@]}"; do
  if docker network inspect "$n" >/dev/null 2>&1; then docker network rm "$n" >/dev/null; fi
done

left_c="$(docker ps -aq --filter "$label")"
left_v="$(docker volume ls -q --filter "$label")"
left_n="$(docker network ls -q --filter "$label")"
for n in "${networks[@]}"; do
  if docker network inspect "$n" >/dev/null 2>&1; then left_n+=" $n"; fi
done
rc=0
if [ -n "$left_c" ]; then echo "teardown: FAIL containers of $project left: $(printf '%s' "$left_c" | tr '\n' ' ')" >&2; rc=1; fi
if [ -n "${left_n// /}" ]; then echo "teardown: FAIL networks of $project left: $left_n" >&2; rc=1; fi
if [ -n "$left_v" ]; then
  if [ "$volumes" -eq 1 ]; then
    echo "teardown: FAIL volumes of $project left: $(printf '%s' "$left_v" | tr '\n' ' ')" >&2; rc=1
  else
    echo "teardown: kept the volumes of $project (no --volumes): $(printf '%s' "$left_v" | tr '\n' ' ')"
  fi
fi
[ "$rc" -eq 0 ] || exit 1
echo "teardown: verified: no container or network of $project is left$([ "$volumes" -eq 1 ] && echo ', and no volume')"
