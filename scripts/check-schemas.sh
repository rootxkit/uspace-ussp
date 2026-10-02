#!/usr/bin/env bash
# Validates every schema under schemas/ as JSON Schema 2020-12 and
# checks its examples both ways (docs/PLAN.md §10): schemas/schemas_test.go
# (each valid example validates, each one under examples/invalid/ is
# refused, $id and title as spec 04 §1). The Go types round-trip the
# valid examples in their own packages' tests (internal/intent). Fails
# when no schema was checked: a run that validated nothing proves
# nothing.
set -euo pipefail
cd "$(dirname "$0")/.."

GO=${GO:-go}
out=$("$GO" test -count=1 -v -run 'TestSchemasAndExamplesBothWays' ./schemas/ 2>&1) || { echo "$out"; exit 1; }
summary=$(printf '%s\n' "$out" | grep -E '[0-9]+ schemas, [0-9]+ valid examples validated' || true)
if [ -z "$summary" ]; then
  echo "$out"
  echo "check-schemas: no schema was validated"
  exit 1
fi
"$GO" test -count=1 -run 'SchemaExamplesRoundTrip' ./internal/intent/ >/dev/null
"$GO" test -count=1 -run 'DecoderAgreesWithTheSchemaExamples|MessagesValidateAgainstTheirSchemas' ./internal/telemetry/ >/dev/null
"$GO" test -count=1 -run 'EventsValidateAndDecode' ./internal/flights/ >/dev/null
echo "check-schemas:${summary#*schemas_test.go:*:}; the intent, telemetry and flight examples and messages round-trip through the Go types"
