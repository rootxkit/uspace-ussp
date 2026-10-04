# Dependencies

Standard library first. Every direct dependency has a row here and a
one-line reason in the commit body that adds it (CLAUDE.md engineering
rules); the allowed list is `docs/PLAN.md` §14. Versions are the ones
`go.mod`, `go.sum` and `web/pnpm-lock.yaml` pin.

## Go modules

| Module | Version | Added by | Why |
|---|---|---|---|
| `github.com/rootxkit/uspace-core` | v1.4.0 | WP-0 | every judgement the ecosystem shares (CLAUDE.md rule 3); `auth` verifies tokens (`StrictSessionClaims`, `Audiences`) and signs session tokens (`Issuer.IssueSession`, v1.2.0), `core` carries `FieldError` and `Counters`, `geoid.LoadMapped` and `terrain.MappedDirOpener` (v1.4.0) map the grids read-only |
| `github.com/jackc/pgx/v5` | v5.11.0 | WP-0 | PostgreSQL + PostGIS and TimescaleDB driver and pool (`internal/store` only); the readiness probes of both databases |
| `github.com/nats-io/nats.go` | v1.54.0 | WP-0 | NATS JetStream client (`internal/bus` only); the connection that reconnects forever and its readiness probe |
| `github.com/prometheus/client_golang` | v1.24.1 | WP-0 | `/metrics`: `ussp_build_info`, `ussp_dependency_*`, the counters (E-09) |
| `go.opentelemetry.io/otel`, `otel/sdk`, `otel/trace`, `otel/exporters/otlp/otlptrace/otlptracehttp` | v1.46.0 | WP-0 | tracing with the OTLP/HTTP exporter when `USSP_OTLP_URL` is set, a no-op provider otherwise |
| `github.com/oapi-codegen/oapi-codegen/v2` (tool) | v2.8.0 | WP-0 | generates `internal/national/gen` and `internal/national/client` from `api/openapi.yaml` (D4), and since WP-3 `internal/stdapi/f3411` and `f3548` from `api/standards/` (D3); a `tool` directive, run with `go tool`, never linked into a binary |
| `github.com/pressly/goose/v3` | v3.28.0 | WP-1 | the two embedded migration trees with their own version tables and session advisory locks (`internal/store` only, run only by the `migrate` subcommand); raises the minimum `procfs`, `grpc` and `genproto/rpc` versions by a patch |
| `github.com/sqlc-dev/sqlc` (generator) | v1.31.1 | WP-1 | generates `internal/store/relational` and `internal/store/timeseries` from the trees and `internal/store/queries`; run by `scripts/generate.sh` with `go run …@v1.31.1` (pinned there), never linked into a binary and not a `go.mod` tool, so its dependency tree stays out of the module |
| `github.com/lestrrat-go/jwx/v3` | (through core) | — | JWT/JWS inside `uspace-core/auth`; never imported here (depguard) |
| `golang.org/x/crypto` | v0.57.0 | WP-2 | argon2id for passwords and client secrets (`internal/auth` Hasher); already in the build list through core |
| `github.com/coder/websocket` | v1.8.15 | WP-2 | the WebSocket upgrade of M22 (`internal/auth` WSAuth: cookie + `Origin`, close 4401); the streams of WP-8 and WP-11 use the same library |
| `github.com/oapi-codegen/runtime` | v1.7.0 | WP-2 | path-parameter binding of the generated server and client (`internal/national/gen`, `client`), needed from the first operation with a path parameter; since WP-3 also the strict-server middleware of `internal/stdapi` |
| `go.yaml.in/yaml/v3` | v3.0.5 | WP-3 | tests only: reads the `security` blocks of the pinned standard OpenAPI files to compare them with the mounted scopes (`internal/stdapi`); already in the build list through oapi-codegen |
| `github.com/santhosh-tekuri/jsonschema/v6` | v6.0.2 | WP-2 | tests only: validates problem bodies against the pinned lab `problem/v1` schema (`internal/national/testdata`); already in the build list through oapi-codegen |

Planned by `docs/PLAN.md` §14 and added by the work package that first
needs them, each with its row: `google/uuid` (not needed by WP-2: the
database makes the ids).

## web/ (pnpm, exact pins)

| Package | Version | Why |
|---|---|---|
| `@rootxkit/uspace-ui` | 0.1.0 (GitHub Release asset, sha512 in `pnpm-lock.yaml`, attestation verified) | the shared kit: theme, fonts (Noto Sans and Noto Sans Georgian via `next/font/local`), map and layers, legends, live client, form, table, alerts, BFF and session helpers, ESLint rules (WP-17) |
| `next`, `react`, `react-dom` | 16.3.8, 19.3.0, 19.3.0 | the portal (App Router, standalone output); React 19.3 as the kit is tested with |
| `maplibre-gl` | 5.24.0 | the kit's map peer (WP-17) |
| `react-hook-form`, `zod` | 7.89.0, 4.6.5 | the kit's `form` peers: the shape of a form; the API judges the meaning (WP-17) |
| `server-only` | 0.0.1 | keeps the server modules out of the browser bundle |
| `@playwright/test` (dev) | 1.63.0 | the browser tests of the operator flows against the e2e stack |
| `axe-core` (dev) | 4.13.0 | the WCAG 2.2 AA checks of the browser tests, injected without a network |
| `eslint`, `typescript-eslint`, `eslint-plugin-react-hooks`, `eslint-plugin-jsx-a11y` (dev) | 9.39.5, 8.71.0, 7.1.1, 6.10.2 | the kit's ESLint config and its peers (replace `eslint-config-next`, WP-17) |
| `tailwindcss`, `@tailwindcss/postcss`, `typescript`, `@types/*` (dev) | see `web/package.json` | styles over the kit's tokens, strict types |

`@fontsource/noto-sans-georgian` and `openapi-typescript` left with
WP-17: the kit bundles the fonts and pins openapi-typescript in its
`uspace-ui-gen-api`.
