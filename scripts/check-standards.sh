#!/usr/bin/env bash
# Offline check of the pinned standard files (brief WP-3): every file
# api/standards/SOURCE names exists, hashes to its `sha256`, and its
# `commit`, `url` and `sha256` equal what uspace-core's SOURCE records
# for the same standard (spec_commit, spec_url, spec_sha256) at the core
# version go.mod pins; every *.yaml under api/standards/ has a SOURCE
# section. Network is never used.
set -euo pipefail
cd "$(dirname "$0")/.."
# shellcheck source=scripts/standards-source.sh
. scripts/standards-source.sh

status=0
bad=0
fail() { echo "check-standards: $*" >&2; status=1; bad=1; }

mapfile -t files < <(source_files)
if [ "${#files[@]}" -eq 0 ]; then
  fail "api/standards/SOURCE names no file"
fi
for f in "${files[@]}"; do
  bad=0
  path="api/standards/$f"
  want=$(source_get "$f" sha256)
  if [ ! -f "$path" ]; then
    fail "$path is named in SOURCE but missing"
    continue
  fi
  got=$(sha256_of "$path")
  if [ "$got" != "$want" ]; then
    fail "$path hashes to $got, SOURCE records $want (edited by hand? run scripts/fetch-standards.sh)"
  fi
  for pair in commit:spec_commit url:spec_url sha256:spec_sha256; do
    ours=$(source_get "$f" "${pair%%:*}")
    core=$(core_get "$f" "${pair#*:}") || { fail "$f: uspace-core's SOURCE not readable"; continue; }
    if [ "$ours" != "$core" ]; then
      fail "$f: ${pair%%:*} is $ours, uspace-core's SOURCE has ${pair#*:} = $core"
    fi
  done
  if [ "$bad" -eq 0 ]; then
    echo "check-standards: $f sha256 $got = SOURCE = uspace-core's spec_sha256, same commit and url"
  fi
done
shopt -s nullglob
for p in api/standards/*.yaml; do
  case "$p" in api/standards/oapi-codegen.*) continue ;; esac
  name=${p#api/standards/}
  if ! printf '%s\n' "${files[@]}" | grep -qxF "$name"; then
    fail "$p has no section in api/standards/SOURCE"
  fi
done
exit "$status"
