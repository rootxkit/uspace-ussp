#!/usr/bin/env bash
# Regenerates the InterUSS automated-testing interface code (docs/PLAN.md
# D13, §6.4; brief WP-21) from the pinned files in api/standards/interuss/
# (SOURCE, sha256 checked by scripts/check-standards.sh):
#
#   rid/v1/{commons,injection}.yaml      -> internal/stdapi/interuss/ridinjection/   (types)
#   rid/v1/commons.yaml                  -> internal/stdapi/interuss/ridobservation/ (types)
#   rid/v1/observation.yaml              -> internal/stdapi/interuss/ridobservation/ (strict server)
#   flight_planning/v1/<sibling>.yaml    -> internal/stdapi/interuss/flightplanning/ (types)
#   flight_planning/v1/flight_planning.yaml -> internal/stdapi/interuss/flightplanning/ (strict server)
#
# Each file is generated into <package>/<file>.gen.go with the package's
# configuration api/standards/interuss/oapi-codegen.<package>[.<side>].yaml.
# oapi-codegen runs through `go tool` at the go.mod pin. OUT_DIR
# (optional) writes into a scratch tree, for scripts/check-generated.sh.
# Sourced or run from scripts/generate.sh.
set -euo pipefail

GO=${GO:-go}
root=$(cd "$(dirname "$0")/.." && pwd)
out=${OUT_DIR:-$root}
src=api/standards/interuss
dst=internal/stdapi/interuss
cd "$root"

# one PACKAGE CONFIG FILE: generate FILE (relative to $src) into
# $dst/PACKAGE/<basename>.gen.go with $src/oapi-codegen.CONFIG.yaml.
# oapi-codegen resolves a file's relative references against the file,
# so it is handed the pinned copy in place.
one() {
  local pkg=$1 config=$2 file=$3 base
  base=$(basename "$file" .yaml)
  mkdir -p "$out/$dst/$pkg"
  "$GO" tool oapi-codegen -config "$src/oapi-codegen.$config.yaml" -o "$out/$dst/$pkg/$base.gen.go" "$src/$file"
  echo "generated: $dst/$pkg/$base.gen.go"
}

one ridinjection ridinjection rid/v1/commons.yaml
one ridinjection ridinjection rid/v1/injection.yaml
one ridobservation ridobservation.models rid/v1/commons.yaml
one ridobservation ridobservation.server rid/v1/observation.yaml
for f in "$src"/flight_planning/v1/*.yaml; do
  name=$(basename "$f")
  if [ "$name" = flight_planning.yaml ]; then
    one flightplanning flightplanning.server "flight_planning/v1/$name"
  else
    one flightplanning flightplanning.models "flight_planning/v1/$name"
  fi
done
