# Dependencies

Standard library first. Every direct dependency has a row here and a
one-line reason in the commit body that adds it (CLAUDE.md engineering
rules); the allowed list is `docs/PLAN.md` §14. Versions are the ones
`go.mod`, `go.sum` and `web/pnpm-lock.yaml` pin.

## Go modules

| Module | Version | Added by | Why |
|---|---|---|---|
| `github.com/rootxkit/uspace-core` | v1.1.0 | WP-0 | every judgement the ecosystem shares (CLAUDE.md rule 3); `auth` verifies tokens (`StrictSessionClaims`, `Audiences`), `core` carries `FieldError` and `Counters` |
| `github.com/jackc/pgx/v5` | v5.11.0 | WP-0 | PostgreSQL + PostGIS and TimescaleDB driver and pool (`internal/store` only); the readiness probes of both databases |
| `github.com/nats-io/nats.go` | v1.54.0 | WP-0 | NATS JetStream client (`internal/bus` only); the connection that reconnects forever and its readiness probe |
| `github.com/prometheus/client_golang` | v1.24.1 | WP-0 | `/metrics`: `ussp_build_info`, `ussp_dependency_*`, the counters (E-09) |
| `go.opentelemetry.io/otel`, `otel/sdk`, `otel/trace`, `otel/exporters/otlp/otlptrace/otlptracehttp` | v1.46.0 | WP-0 | tracing with the OTLP/HTTP exporter when `USSP_OTLP_URL` is set, a no-op provider otherwise |
| `github.com/oapi-codegen/oapi-codegen/v2` (tool) | v2.8.0 | WP-0 | generates `internal/national/gen` and `internal/national/client` from `api/openapi.yaml` (D4); a `tool` directive, run with `go tool`, never linked into a binary |
| `github.com/pressly/goose/v3` | v3.28.0 | WP-1 | the two embedded migration trees with their own version tables and session advisory locks (`internal/store` only, run only by the `migrate` subcommand); raises the minimum `procfs`, `grpc` and `genproto/rpc` versions by a patch |
| `github.com/sqlc-dev/sqlc` (generator) | v1.31.1 | WP-1 | generates `internal/store/relational` and `internal/store/timeseries` from the trees and `internal/store/queries`; run by `scripts/generate.sh` with `go run …@v1.31.1` (pinned there), never linked into a binary and not a `go.mod` tool, so its dependency tree stays out of the module |
| `github.com/lestrrat-go/jwx/v3` | (through core) | — | JWT/JWS inside `uspace-core/auth`; never imported here (depguard) |
| `golang.org/x/crypto` | v0.57.0 | WP-2 | argon2id for passwords and client secrets (`internal/auth` Hasher); already in the build list through core |
| `github.com/coder/websocket` | v1.8.15 | WP-2 | the WebSocket upgrade of M22 (`internal/auth` WSAuth: cookie + `Origin`, close 4401); the streams of WP-8 and WP-11 use the same library |
| `github.com/oapi-codegen/runtime` | v1.7.0 | WP-2 | path-parameter binding of the generated server and client (`internal/national/gen`, `client`), needed from the first operation with a path parameter |
| `github.com/santhosh-tekuri/jsonschema/v6` | v6.0.2 | WP-2 | tests only: validates problem bodies against the pinned lab `problem/v1` schema (`internal/national/testdata`); already in the build list through oapi-codegen |

Planned by `docs/PLAN.md` §14 and added by the work package that first
needs them, each with its row: `google/uuid` (not needed by WP-2: the
database makes the ids).

## web/ (npm, exact pins)

| Package | Version | Why |
|---|---|---|
| `next`, `react`, `react-dom` | 16.3.8, 19.2.8, 19.2.8 | the portal and console (App Router, standalone output) |
| `@fontsource/noto-sans-georgian` | 5.3.0 | Noto Sans Georgian woff2 files, bundled through `next/font/local` (no third-party font request) |
| `server-only` | 0.0.1 | marks the BFF fetch module so it can never reach the browser bundle |
| `openapi-typescript` (dev) | 7.13.0 | `src/api/types.ts` from `api/openapi.yaml` |
| `@playwright/test` (dev) | 1.63.0 | the smoke test of the one page in `ka` and `en` |
| `tailwindcss`, `@tailwindcss/postcss`, `eslint`, `eslint-config-next`, `typescript`, `@types/*` (dev) | see `web/package.json` | the `create-next-app` toolchain: styles, lint rules, strict types |

`@rootxkit/uspace-ui` joins with an exact pin when it publishes
`0.1.0-rc` (M32); see `web/README.md`.
