#!/usr/bin/env bash
# The import order of docs/PLAN.md §4: internal packages import each
# other only downward, layer by layer; a package may import its own
# subpackages. internal/app (the process wiring) sits above every layer
# and is imported only by cmd/; nothing imports cmd/. A package outside
# the table fails until it is added to it here and in the plan. Go
# itself refuses cycles; this enforces the direction.
set -euo pipefail
cd "$(dirname "$0")/.."
GO=${GO:-go}
module="$("$GO" list -m)"

declare -A layer=(
  [config]=0 [obs]=0 [httpx]=0 [cell]=0 [policy]=0
  [store]=1 [bus]=1 [auth]=1 [stdapi]=1
  [cis]=2 [registry]=2 [sources]=2 [accounts]=2
  [intent]=3 [flights]=3 [telemetry]=3
  [conformance]=4 [traffic]=4 [geo]=4 [alerts]=4 [ridsp]=4 [dss]=4 [peers]=4 [manned]=4
  [coordination]=5 [records]=5 [occurrence]=5 [status]=5 [weather]=5 [admin]=5 [national]=5
  [app]=6 [testfakes]=6
)

top() { # the first path element under internal/, or "" outside internal/
  local p=${1#"$module"/}
  case "$p" in internal/*) p=${p#internal/}; echo "${p%%/*}" ;; *) echo "" ;; esac
}

# Listed first, so a package that does not build fails the script
# instead of leaving the loop below with nothing to check.
listing="$("$GO" list -f '{{.ImportPath}} {{join .Imports " "}}' ./...)"

status=0
checked=0
while read -r pkg imports; do
  checked=$((checked + 1))
  from="$(top "$pkg")"
  if [ -n "$from" ] && [ -z "${layer[$from]+x}" ]; then
    echo "check-deps: internal/$from is not in the import order (docs/PLAN.md §4)" >&2
    status=1
    continue
  fi
  for imp in $imports; do
    case "$imp" in "$module"/*) ;; *) continue ;; esac
    if [[ "$imp" == "$module"/cmd/* ]]; then
      echo "check-deps: $pkg imports $imp; nothing imports cmd/" >&2
      status=1
      continue
    fi
    to="$(top "$imp")"
    [ -z "$to" ] && continue
    if [ -z "$from" ]; then
      # cmd/ and test/ may import internal/app and the layers; cmd/
      # reaches the processes only through internal/app.
      continue
    fi
    [ "$from" = "$to" ] && continue
    if [ -z "${layer[$to]+x}" ]; then
      echo "check-deps: internal/$to is not in the import order (docs/PLAN.md §4)" >&2
      status=1
    elif [ "${layer[$to]}" -ge "${layer[$from]}" ]; then
      echo "check-deps: internal/$from (layer ${layer[$from]}) imports internal/$to (layer ${layer[$to]}); imports go downward only" >&2
      status=1
    fi
  done
done <<<"$listing"

if [ "$checked" -eq 0 ]; then
  echo "check-deps: no package listed; nothing was checked" >&2
  exit 1
fi
if [ "$status" -eq 0 ]; then
  echo "check-deps: $checked packages follow the import order of docs/PLAN.md §4"
fi
exit "$status"
