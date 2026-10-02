// Package schemas holds no code. It exists so that `go test ./...` and
// scripts/check-schemas.sh run schemas_test.go, which validates every
// JSON Schema this repository owns (docs/PLAN.md D8: telemetry, alert,
// intent/*, traffic/product) as draft 2020-12 and every example under
// <name>/v<major>/examples/ in both directions: each valid example
// validates and each one under examples/invalid/ is refused. The layout
// is uspace-lab's schemas/ (spec 04 §1: $id
// https://schemas.uspace.ge/<name>/v<major>.json, title <name>/v<major>),
// so the lab mirrors this directory byte for byte. The Go types
// round-trip the valid examples in their own packages (internal/intent).
package schemas
