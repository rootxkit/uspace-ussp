#!/usr/bin/env bash
# Regenerates every generated Go file into a scratch directory and fails
# on any difference from the committed copy, after checking that the
# pinned standard files are the ones api/standards/SOURCE records
# (scripts/check-standards.sh). Offline: the generator comes from the
# module cache (go.mod tool directive) and the standard files are the
# vendored copies.
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT
# Git Bash on Windows: hand the Windows go binary a path it can open.
if command -v cygpath >/dev/null 2>&1; then scratch=$(cygpath -m "$scratch"); fi

GO=${GO:-go} "$root/scripts/check-standards.sh"

files=(internal/national/gen/server.gen.go internal/national/client/client.gen.go internal/telemetry/gen/server.gen.go
  internal/ridsp/gen/server.gen.go internal/traffic/gen/server.gen.go
  internal/stdapi/f3411/server.gen.go internal/stdapi/f3411/client.gen.go
  internal/stdapi/f3548/server.gen.go internal/stdapi/f3548/client.gen.go
  internal/cis/cispclient/client.gen.go internal/registry/authclient/client.gen.go
  internal/status/authclient/client.gen.go internal/occurrence/authclient/client.gen.go
  internal/coordination/anspclient/client.gen.go)
# The sqlc packages are compared file by file, both ways: a stale file
# left behind is as wrong as a missing one.
sqlc_dirs=(internal/store/relational internal/store/timeseries)

OUT_DIR="$scratch" "$root/scripts/generate.sh" >/dev/null

status=0
for f in "${files[@]}"; do
  if [ ! -f "$root/$f" ]; then
    echo "missing committed file: $f (run make generate)"
    status=1
    continue
  fi
  if ! diff -u "$root/$f" "$scratch/$f"; then
    echo "out of date: $f (run make generate and commit the result)"
    status=1
  fi
done
for d in "${sqlc_dirs[@]}"; do
  if ! diff -ru "$root/$d" "$scratch/$d"; then
    echo "out of date: $d (run make generate and commit the result)"
    status=1
  fi
done
if [ "$status" -eq 0 ]; then
  echo "check-generated: ${#files[@]} files and ${#sqlc_dirs[@]} sqlc packages up to date: ${files[*]} ${sqlc_dirs[*]}"
fi
exit "$status"
