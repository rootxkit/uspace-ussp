#!/usr/bin/env bash
# Regenerates every generated Go file into a scratch directory and fails
# on any difference from the committed copy. Offline: the generator
# comes from the module cache (go.mod tool directive), nothing is
# compared against another repository.
set -euo pipefail

root=$(cd "$(dirname "$0")/.." && pwd)
scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT
# Git Bash on Windows: hand the Windows go binary a path it can open.
if command -v cygpath >/dev/null 2>&1; then scratch=$(cygpath -m "$scratch"); fi

files=(internal/national/gen/server.gen.go internal/national/client/client.gen.go)

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
if [ "$status" -eq 0 ]; then
  echo "check-generated: ${#files[@]} files up to date: ${files[*]}"
fi
exit "$status"
