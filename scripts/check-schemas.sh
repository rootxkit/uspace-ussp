#!/usr/bin/env bash
# Validates every schema under schemas/ as JSON Schema 2020-12 and
# round-trips its examples (docs/PLAN.md §10). WP-8 adds the first
# schema and, with it, the validator. Until then there is nothing to
# validate and the script says so; a schema that appears without the
# validator fails, so a schema is never reported as checked when it
# was not.
set -euo pipefail
cd "$(dirname "$0")/.."

shopt -s globstar nullglob
schemas=(schemas/**/*.json)
if [ "${#schemas[@]}" -eq 0 ]; then
  echo "check-schemas: no schema under schemas/ yet (WP-8 adds the first); nothing to validate"
  exit 0
fi
echo "check-schemas: ${#schemas[@]} schema files but no validator yet: WP-8 must add it with the first schema" >&2
printf '  %s\n' "${schemas[@]}" >&2
exit 1
