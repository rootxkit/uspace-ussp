#!/usr/bin/env bash
# Re-fetches every standard file api/standards/SOURCE names from its
# `url` and refuses one whose SHA-256 is not `sha256`; with -check it
# only compares the fetched bytes with the vendored copy; -interuss (first)
# does the same for api/standards/interuss/SOURCE (brief WP-21). Needs network;
# CI never runs it (scripts/check-standards.sh is the offline check).
set -euo pipefail
cd "$(dirname "$0")/.."
dir=api/standards
if [ "${1:-}" = "-interuss" ]; then
  dir=api/standards/interuss
  source_path=$dir/SOURCE
  shift
fi
# shellcheck source=scripts/standards-source.sh
. scripts/standards-source.sh

check=false
if [ "${1:-}" = "-check" ]; then check=true; fi
scratch=$(mktemp -d)
trap 'rm -rf "$scratch"' EXIT

status=0
while IFS= read -r f; do
  url=$(source_get "$f" url)
  want=$(source_get "$f" sha256)
  mkdir -p "$(dirname "$scratch/$f")"
  curl -fsSL --max-time 60 -o "$scratch/$f" "$url"
  got=$(sha256_of "$scratch/$f")
  if [ "$got" != "$want" ]; then
    echo "fetch-standards: $url hashes to $got, SOURCE records $want: not written" >&2
    status=1
    continue
  fi
  if $check; then
    if ! cmp -s "$scratch/$f" "$dir/$f"; then
      echo "fetch-standards: $dir/$f differs from $url" >&2
      status=1
      continue
    fi
    echo "fetch-standards: $dir/$f equals $url ($got)"
  else
    mkdir -p "$(dirname "$dir/$f")"
  cp "$scratch/$f" "$dir/$f"
    echo "fetch-standards: wrote $dir/$f ($got)"
  fi
done < <(source_files)
exit "$status"
