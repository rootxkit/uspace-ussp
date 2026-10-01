# WP-0: scaffold

Branch `feat/WP-0-scaffold`. Milestone S-M0. Owns `go.mod`, `go.sum`,
`cmd/*` (skeletons), `internal/config`, `internal/obs`, `internal/httpx`,
`api/openapi.yaml` (skeleton), `scripts/`, `Makefile`, `.golangci.yml`,
`.github/workflows/`, `deploy/Dockerfile`, `deploy/compose/`, `SECURITY.md`,
`CHANGELOG.md`, `docs/DEPENDENCIES.md`, `docs/bench-targets.txt`, `web/`
(bootstrap only). Depends on nothing. Every later WP inherits what this
one freezes.

## Read first

1. `CLAUDE.md`, `docs/PLAN.md §2` (D1, D4, D5), `§4`, `§11`, `§14`.
2. `uspace-core` `.github/workflows/ci.yml`, `Makefile`, `.golangci.yml`,
   `docs/WORKPACKAGES/WP-0.md` (what its first CI run caught: missing doc
   comments, `fmt.Printf` in scripts, gosec G304, gitleaks on fixtures,
   scripts without the executable bit). Avoid the same four.
3. Spec `00 §6.1` (the seven processes), `05 §6` (what keeps running when
   what fails), `06 §4` (public repo rules).
4. LESSONS E-02 (health paths run deliberately), B-08 (reconnect forever,
   start degraded), E-09 (count everything).

## What to build

- `go.mod`: `module github.com/rootxkit/uspace-ussp`, `go 1.27`,
  `require github.com/rootxkit/uspace-core v1.0.0`. No other dependency
  until a package needs it; each one gets a row in `docs/DEPENDENCIES.md`
  (the allowed list is in `PLAN.md §14`) and a reason in the commit body.
- `cmd/api`, `cmd/telemetry-ingest`, `cmd/rid-sp`, `cmd/monitor`,
  `cmd/traffic-ws`, `cmd/dss-sync`, `cmd/tsdb-writer`: each a `main.go`
  that loads config, sets up `obs`, serves `/healthz`, `/readyz`,
  `/metrics` on its port, handles SIGTERM with a drain, and logs one
  structured start line (`process`, `version`, `commit`, config summary
  without secrets). `cmd/ussp-dev` runs all seven `Run(ctx, cfg)` functions
  in one process (each `cmd/<p>` exposes its `Run` from
  `internal/app/<p>`; `main.go` is ten lines).
- `internal/config`: `Load() (Config, error)` from env (`USSP_*`, PLAN
  §11), typed, with units in names (`TelemetryLostS`), defaults, and a
  `Redacted()` for logs. Every variable documented in `deploy/ENV.md`
  (process that reads it, default, unit). Missing required → the process
  exits 2 with the variable named; nothing else exits.
- `internal/obs`: `slog` JSON handler with `process`, `request_id`,
  `drone_id`/`flight_id`/`intent_id`/`client_id` as typed attrs helpers;
  Prometheus registry with `ussp_build_info`, `ussp_dependency_up{dep}`,
  `ussp_dependency_age_s{dep}`; OpenTelemetry tracer provider (OTLP over
  HTTP when `USSP_OTLP_URL` set, no-op otherwise); `Health` registry where
  each dependency reports `state` (`up`, `degraded`, `down`, `unknown`),
  `since`, `age_s`, `detail`. `/readyz` renders it as JSON and returns 503
  while any required dependency is `down` and 200 with `degraded` listed
  otherwise (E-02: write the test that takes a dependency away and reads
  the body).
- `internal/httpx`: `NewServer` with read/write/idle timeouts, header and
  body caps (1 MiB default, per-route override), request id, recovery
  that logs and returns problem+json 500 (never a panic trace to the
  client), `Problem(w, status, type, detail, extra)`, scope middleware
  stub (`RequireScope(scope)` taking a verifier interface WP-2 fills),
  `Bearer(r)`.
- `api/openapi.yaml`: OpenAPI 3.1 skeleton with `info`, `servers`
  (variable host), `components.securitySchemes` (`operatorToken`,
  `ecosystemToken`, `session`), the `Problem` schema in the shared
  shape (`type`, `title`, `status`, `detail`, `instance`, `errors:
  [{field, reason}]`, `truncated`; `type` = `https://schemas.uspace.ge/problems/<slug>`;
  reconciliation M28, mirrored from `uspace-lab/schemas/common/problem/v1`
  once it exists), the tags of PLAN §6.1
  (empty), `GET /healthz`, `GET /readyz`. `api/clients/` directory with
  a `README` describing the sibling-copy mechanism (`<system>.yaml` +
  `SOURCE`; M11) and `scripts/check-contracts.sh` that diffs each copy
  against its repo at the `SOURCE` commit (no copies yet; the script
  passes on an empty directory and says so). `oapi-codegen` config
  (`api/oapi-codegen.server.yaml`, `client.yaml`) generating
  `internal/national/gen/` (`std-http-server`, strict server, Go 1.22
  routing) and `internal/national/client/`. `scripts/check-generated.sh`
  regenerates into a temp dir and diffs; CI runs it offline (the
  generator is a `go run` tool pinned in `go.mod`'s tool directive).
- `Makefile`: `build vet fmt lint tools race test cover integration
  generate check-generated check-schemas check-deps secrets vulncheck
  image compose-up compose-down conformance ci clean`, with the linter
  versions pinned as in core (golangci-lint v2.14.0, staticcheck v0.8.1,
  govulncheck v1.8.0, gitleaks 8.24.3) and `make lint` refusing another
  golangci-lint version.
- `.golangci.yml`: core's, with `forbidigo` adapted: `log/slog` allowed;
  `fmt.Print*`, `log.*`, `os.Exit` forbidden outside `cmd/*/main.go`,
  `scripts/` and tests; `panic` forbidden everywhere but tests.
- `.github/workflows/ci.yml` as PLAN §14: jobs `build-vet-lint`,
  `test-race`, `integration` (service containers
  `timescale/timescaledb-ha:pg16` and `nats:2-alpine`; the integration
  tag runs a smoke test that connects, runs no migration yet, and asserts
  `/readyz` reports both `up`), `web`, `image` (build only on PRs),
  `gitleaks`, `govulncheck`; `concurrency` with cancel-in-progress;
  `timeout-minutes` per job; `paths-ignore: ['docs/**', '*.md']` on the Go
  jobs and `paths: ['web/**', 'api/**']` on `web`. No schedule. A
  `conformance` workflow with `workflow_dispatch` only, calling
  `make conformance` (fails until WP-19 fills it; leave it out of required
  checks).
- `deploy/Dockerfile`: multi-stage, `CGO_ENABLED=0`, all seven binaries,
  distroless static, non-root; `deploy/web.Dockerfile` for Next.js
  standalone. `deploy/compose/docker-compose.yml`: the seven processes
  from one image with entrypoints, `web`, one `timescaledb` container
  (`timescale/timescaledb-ha:pg16`, PostGIS included) with an init
  script creating both databases, `ussp_relational` and
  `ussp_timeseries` (reconciliation M37; the two migration trees stay
  separate), a one-shot `migrate` service (empty until WP-1; M36),
  `nats` (JetStream, a
  `nats.conf` with one account per process and no auth-less access),
  healthchecks, an isolated network, `.env.example`. Profiles `demo` and
  `conformance` declared, empty.
- `scripts/check-deps.sh` (the import order of PLAN §4 via `go list -deps`),
  `scripts/check-schemas.sh` (stub: validates `schemas/*.json` as JSON
  Schema 2020-12 and round-trips examples when WP-8 adds the first),
  `scripts/check-hostnames.sh` (grep `chikox.net` outside `deploy/staging/`).
- `web/`: `create-next-app` (App Router, TypeScript strict, Tailwind),
  `pnpm` with `packageManager` pinned and `pnpm-lock.yaml` committed
  (`--frozen-lockfile` in CI and the image; M34), `uspace-ui` added
  from npmjs when it publishes its `0.1.0-rc` (exact pin, never a git
  tag; M32; until then a `// KIT: pending uspace-ui` marker in
  `web/README.md`), `openapi-typescript`
  generating `web/src/api/types.ts` from `api/openapi.yaml`, ESLint rules:
  no hand-written type under `src/api/`, no import of `turf`, `proj4`,
  `geolib`, `h3-js`, `@turf/*`, `cheap-ruler` (the no-geometry rule), no
  `pg`/`nats` import; `ka` and `en` catalogues with Noto Sans Georgian;
  one page `/` rendering `/readyz` of the BFF target. Playwright smoke
  that opens it.
- `SECURITY.md` (90-day disclosure), `CHANGELOG.md` (Keep a Changelog,
  Unreleased), `docs/DEPENDENCIES.md`, `docs/bench-targets.txt` with the
  names of PLAN §9 and their targets.

## Done when

- [ ] `make lint` with the pinned linters prints no issue; `go build ./...`,
  `go vet ./...`, `go mod tidy` clean; `scripts/check-generated.sh` passes
  offline.
- [ ] `make integration` green in CI against the service containers
  (`/readyz` reports `postgres`, `timescaledb`, `nats` up); the test that
  stops NATS (or points at a closed port) reads `/readyz` and finds
  `nats: down` and a 503 (E-02).
- [ ] `docker build` of both images succeeds in CI; `make compose-up`
  brings up every container healthy on a laptop; `make compose-down`
  leaves nothing behind and exits 0 (E-02: the success path of teardown).
- [ ] The first CI run's failures, if any, fixed on the branch and listed
  in the PR, as core's WP-0 did.
- [ ] `web` job green; `ka`/`en` render the one page.
- [ ] `CHANGELOG.md` line; PR with the outputs pasted (E-04).

## Safety notes

- Nothing here talks to an aircraft, a DSS or a peer; keep it so. Any
  outbound HTTP client lives in a later WP with its scope.
- Do not add a dependency "for later". The owner reviews each one.
- Write the health paths to be read, not to pass: the `/readyz` body is
  what a supervisor sees at 3 a.m.

## Commits

`build: module, processes and health endpoints [WP-0 S-M0]`,
`ci: lint, race, integration services, images, gitleaks [WP-0 S-M0]`,
`build(deploy): images and compose for the seven processes [WP-0 S-M0]`,
`feat(web): bootstrap the portal shell with generated types and i18n [WP-0 S-M0]`.
