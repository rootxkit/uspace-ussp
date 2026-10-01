#!/usr/bin/env bash
# The sibling OpenAPI files this system calls are pinned copies in
# api/clients/ (reconciliation M11): <system>.yaml beside a line in
# api/clients/SOURCE of the form
#
#   <system> <owner/repo> <commit> <path in that repo>
#
# This script fetches each file at its commit and fails on any
# difference, on a copy without a SOURCE line and on a SOURCE line
# without a copy. With no copy yet it passes and says so.
set -euo pipefail
cd "$(dirname "$0")/.."

shopt -s nullglob
copies=(api/clients/*.yaml)
source_file=api/clients/SOURCE
lines=()
if [ -f "$source_file" ]; then
  while IFS= read -r line; do
    case "$line" in ''|'#'*) continue ;; esac
    lines+=("$line")
  done < "$source_file"
fi

if [ "${#copies[@]}" -eq 0 ] && [ "${#lines[@]}" -eq 0 ]; then
  echo "check-contracts: no sibling OpenAPI copy in api/clients/ yet; nothing to compare"
  exit 0
fi

status=0
declare -A pinned=()
for line in "${lines[@]}"; do
  read -r system repo commit path <<<"$line"
  if [ -z "${path:-}" ]; then
    echo "check-contracts: malformed SOURCE line: $line" >&2
    status=1
    continue
  fi
  pinned[$system]=1
  copy="api/clients/$system.yaml"
  if [ ! -f "$copy" ]; then
    echo "check-contracts: SOURCE names $system but $copy is missing" >&2
    status=1
    continue
  fi
  scratch="$(mktemp)"
  if ! curl -fsSL "https://raw.githubusercontent.com/$repo/$commit/$path" -o "$scratch"; then
    echo "check-contracts: cannot fetch $repo@$commit:$path" >&2
    rm -f "$scratch"
    status=1
    continue
  fi
  if diff -u "$scratch" "$copy"; then
    echo "check-contracts: $copy equals $repo@$commit:$path"
  else
    echo "check-contracts: $copy differs from $repo@$commit:$path" >&2
    status=1
  fi
  rm -f "$scratch"
done
for copy in "${copies[@]}"; do
  system="$(basename "$copy" .yaml)"
  if [ -z "${pinned[$system]:-}" ]; then
    echo "check-contracts: $copy has no line in $source_file" >&2
    status=1
  fi
done
exit "$status"
