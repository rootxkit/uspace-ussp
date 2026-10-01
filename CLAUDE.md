# uspace-ussp: rules for every contributor and agent

`uspace-ussp` is the reference U-space Service Provider of the Georgian
U-space system-of-systems (Reg. (EU) 2021/664 Art. 7–13, 15): operator
accounts, network identification (ASTM F3411-22a Service Provider),
flight authorisation and strategic deconfliction (ASTM F3548-21 via an
InterUSS DSS), conformance monitoring, traffic information, geo-awareness
(EUROCAE ED-318 from the CISP), optional weather, records and occurrence
reports to the authority, and the operator portal and USSP console under
`web/`. It is one USSP of possibly many: everything that crosses its
boundary is a standard or a published OpenAPI contract. Read
`docs/PLAN.md` before changing anything; every change belongs to a work
package brief in `docs/WORKPACKAGES/`. The system spec is in
`uspace-lab/docs/spec/`; the lessons and vectors in `uspace-lab/knowledge/`.

## Hard rules

1. **Nothing here commands an aircraft.** No process has a socket,
   credential, message type or dependency with a send path towards a
   vehicle or a ground control station. Authorisation decisions, alerts
   and geo-awareness go to people: the operator's client, the portal,
   the console, the ANSP, peers. The telemetry socket sends only `status`
   frames. A task that seems to need a send path is out of scope: stop
   and ask (LESSONS INV-01).
2. **The USSP informs; it never resolves.** No resolution advice of any
   kind in code, schemas, alerts or UI strings: no "climb", "descend",
   "hold", "turn". The remote pilot decides (Art. 11(4), LESSONS X-15).
3. **A judgement lives once, in `uspace-core`.** Identification, zone
   judgement, CPA, the alert lifecycle, time placement, pressure
   altitude, geodesy, ED-318 parsing, F3411/F3548 validation and JWT
   verification are imported from `github.com/rootxkit/uspace-core`,
   pinned by tag, never copied or re-implemented. The two judgements
   this repo owns because only a USSP makes them, conformance
   (`internal/conformance`) and strategic deconfliction
   (`internal/intent/deconflict`), are pinned by the vectors in
   `testdata/vectors/` and proposed upstream. A second copy of any
   judgement, in Go or in TypeScript, is a review failure (spec `06` T12).
4. **Never grant what conflicts.** No authorisation that overlaps a
   PROHIBITED zone, an active restriction or a higher-priority intent,
   whoever asks. There is no override flag. An input that cannot be
   judged (no geoid, no terrain, stale CIS, registry unknown) refuses or
   holds; it never passes by default (LESSONS E-15, Z-09).
5. **Thresholds are data.** Deviation thresholds, CPA minima, radii,
   silence timeouts, staleness bounds, retention live in the versioned
   `policy` row, and `policy_version` travels on every alert, decision
   and record. Never a literal in a judgement, never relaxed to make a
   test pass (INV-03). The 24 h limits on peer data and the standards'
   constants come from `uspace-core/f3411` and `f3548`, not from policy.
6. **An alert path is not done until SITL or a lab scenario raised and
   cleared it** (INV-02). Conformance, proximity, zone, lost-link and
   restriction alerts each list their scenario in the brief and record
   the run (date, image digest, numbers) in `docs/RUNBOOKS/`.
7. **Nothing is hidden.** A failed input is `unavailable since T`, a
   disabled source is `source_disabled` by whom and when, a stale track
   carries `age_s`, a judgement that did not run says so. An empty
   console must never look like an empty sky (LESSONS B-11, SC-22).
8. **No PII beyond Annex IV and Art. 8.** Registration numbers, serials,
   an emergency contact *reference*, the remote pilot or take-off
   position for the flight, and the USSP's own customer record. No
   names, addresses, phones or emails from the registry; records to the
   authority carry none. The registry's answer is a status, and the USSP
   refuses a response that carries more (spec `06 §1`, `§5`).
9. **Standards and published contracts only.** Every USSP ↔ DSS, USSP ↔
   peer, DP ↔ SP and CIS interface is the standard one; every national
   endpoint is in `api/openapi.yaml` (an endpoint that is not in the
   file does not exist). The CISP, the DSS and the token service are
   configuration; peers are discovered through the DSS; no USSP, CISP or
   hostname is named in code (spec `00 §7`). Wire formats are generated
   from the pinned standard files and the OpenAPI, never written from
   memory (E-03).
10. **Two migration trees, never merged.** `migrations/relational/`
    (PostgreSQL + PostGIS, written only by `api`) and
    `migrations/timeseries/` (TimescaleDB, written only by `tsdb-writer`).
    Hot-path processes never open PostgreSQL; they read NATS KV
    projections with their age (LESSONS B-15, G-08).
11. **Units and datums in every name** (E-13): `alt_amsl_m`, `alt_wgs84_m`,
    `height_m` + `height_ref`, `speed_ms`, `timeout_s`; in Go `AltAMSLM`,
    `SpeedMS`, `TimeoutS` or `time.Duration`. AMSL and AGL never meet in
    one calculation; separation and conformance are judged in AMSL
    (D-01). No stored AGL (D-02). F3548 altitudes stay W84 as given and
    AMSL is derived beside them.
12. **English only** in code, comments, commits and docs. User-facing
    strings go through i18n (`ka`, `en`) from the first page.

## Cross-system contracts (reconciled 2026-10-02)

These were decided across the five plans and are not this repo's to
change; a different need is a PR against the owning repo first.

- **Audience rule.** The `aud` of every machine token is the host of
  the target's published base URL (`uspace-cisp.chikox.net`, the DSS
  host, a peer's `uss_base_url` host). This system accepts the list
  `USSP_AUDIENCES` (its public host plus a lab alias); it requests
  `audience` = the target's host for every outgoing call; it never
  uses `USSP_SYSTEM_ID` (the USSP code from the authority's certificate)
  as an audience. Sessions are JWTs with `aud` = our host, `scope =
  "session"`, `roles[]`, `realm` (`portal` or `console`), verified by
  the same `core/auth.Verifier`; cookies `uspace_session` /
  `uspace_csrf`; WebSockets authenticate with the cookie on a
  same-origin upgrade plus an `Origin` allow-list, never a ticket.
- **Error body.** RFC 9457 `application/problem+json` with `{type,
  title, status, detail, instance, errors: [{field, reason}],
  truncated?}`; `type` = `https://schemas.uspace.ge/problems/<slug>`.
  A refused intent's `conflicts[]` live on `intent/decision/v1`, not on
  the problem.
- **Every WebSocket frame carries the envelope** (`schema`, `msg_id`,
  `producer`, `ts`, `rx_ts`, `captured_at`, `time_source`, `backlog`)
  and a `body` named by `schema`; consumers dispatch on `schema`.
  Browser-facing streams speak the console frame: `console/status/v1`
  every 2 s, `console/snapshot/v1` on connect, `console/subscribe/v1`
  from the client, catalogued messages as bodies.
- **Paths.** CIS change notifications arrive at
  `POST /v1/cis/notifications` (from the CISP or, degraded, the ANSP);
  Annex V notices go to `POST {ansp}/v1/coordination/notices`.
- **Schemas.** We own `telemetry/v1`, `intent/*`, `alert/v1`,
  `traffic/product/v1`; we consume `coordination/annex_v/v1` (ANSP),
  `occurrence/v1` (authority), `cis/*` (CISP), `track/manned/v1`
  (ANSP) and the shared shapes in `uspace-lab/schemas/common/`. Sibling
  OpenAPI files are pinned copies in `api/clients/` with a `SOURCE`
  commit and a CI diff; never hand-built clients.
- **Operations.** `USSP_MTLS_MODE = required | off`; migrations only
  through the `migrate` subcommand and the one-shot compose service;
  `pnpm` in `web/`; `uspace-ui` from npmjs with an exact pin.

## Testing rules (LESSONS E-01 to E-04, E-10, E-11)

- **E-01 Test presence, not only absence.** Every test that asserts
  something does not happen (no alert, no refusal, no drop, no DSS
  write, nil) is paired with the test that makes it happen. Every
  `refuse-*` has its `accept-*` twin.
- **E-02 Run the branch that says nothing is wrong.** Exercise success,
  health and degraded paths deliberately: make the thing succeed and
  read what it says; take the dependency away (CISP, authority, DSS,
  ANSP, NATS, TimescaleDB, geoid, terrain, a KV bucket) and check the
  exact degraded output on `/readyz`, in the product and in the counter.
- **E-03 Never write a wire format from memory.** F3411, F3548 and
  ED-318 shapes come from the pinned files through generated code and
  `uspace-core`; our own messages from `schemas/` and `api/openapi.yaml`
  with examples that round-trip in CI.
- **E-04 Never report an inference as an observation.** "The suite
  passes" means you ran it and read the output; a skipped case is
  reported as skipped; a tool error is not evidence about the thing
  checked; a lab scenario is recorded with its numbers, not described.
- **E-10** every bounded structure (queues, windows, caches, nonce sets,
  rings, grids) has a test that exceeds its bound.
- **E-11** tests restore global state and pass under `-shuffle=on` and
  `-race`.
- Core vectors: run the cases that name `ussp` through **this repo's
  adapters** with `vectors.File.RunOwned(t, "ussp", ...)`, never through
  a second judgement. Repo vectors (`testdata/vectors/*.json`) use the
  lab's file shape and the same harness.
- Integration tests run against real PostgreSQL + PostGIS, TimescaleDB
  and NATS (`make integration`, tag `integration`), with the fakes in
  `internal/testfakes/` for the CISP, the authority, the ANSP, a peer
  USS and the operator client, and `internal/dss/fakedss` for the DSS;
  each fake has a `Down()` switch.
- Coverage: ≥ 85 % statement coverage on `internal/conformance`,
  `internal/intent`, `internal/telemetry`, `internal/ridsp`,
  `internal/dss`, `internal/traffic`, `internal/cis`; every branch that
  produces a distinct counter, reason or problem `type` covered by a
  named test.

## Engineering rules

- Go 1.27, `net/http` with Go 1.22 routing, `log/slog` JSON with
  structured context (`flight_id`, `intent_id`, `client_id`, `cell`),
  Prometheus, OpenTelemetry, config by env (`USSP_*`, documented in
  `deploy/ENV.md`). No cgo. No `panic` outside `main` and tests; no
  `fmt.Print*`; no `os.Exit` outside `main`.
- Dependencies: standard library first; each new module needs a one-line
  reason in the commit body and a row in `docs/DEPENDENCIES.md`.
- Generated code (`internal/stdapi`, `internal/national/gen`,
  `internal/store/*`, `web/src/api/types.ts`) is committed, never edited
  by hand, and verified offline by `scripts/check-generated.sh` in CI.
- `web/` renders only: BFF routes carry the session cookie as a bearer
  and nothing else; no database, no NATS, no geometry library (ESLint
  rule); API types from `openapi-typescript`; `ka` and `en` catalogues
  complete (a missing key fails the build); Noto Sans Georgian bundled
  via `next/font/local`; the kit's CSP; the basemap from `/basemap/`
  served by the deployment; no third-party tile or font request.
- Everything refused, dropped, degraded or late is counted with a
  stable snake_case name and visible on `/metrics` and, where a person
  needs it, on `/readyz` and the console (E-09).
- Public repository: no secret, key, certificate, token or production
  hostname in git; `gitleaks` in CI; fixtures use `GEO-TEST-*` numbers
  and `TEST*` serials; `chikox.net` only under `deploy/staging/`.

## Git conventions

- Branch per work package: `feat/WP-<k>-<slug>` (the slug is in the
  brief). Plan and docs branches: `plan/<slug>`, `docs/<slug>`.
- Conventional Commits, one logical change per commit, imperative subject
  under 72 characters, the work package and milestone in brackets at the
  end: `feat(intent): decide an Annex IV request against the CIS cache [WP-7 S-M1]`,
  `fix(conformance): keep a pressure-altitude sample within the margin [WP-10 S-M2]`,
  `test(ridsp): serve special values for unknown speed and track [WP-9 S-M2]`.
  Types: `feat`, `fix`, `test`, `refactor`, `perf`, `docs`, `build`, `ci`,
  `chore`. Scope is the package or process.
- **No AI attribution of any kind**: no `Co-Authored-By`, no "generated
  by", no tool names in commits, PRs or code.
- Never force-push a shared branch; never commit to `main` directly.
- Do not merge; the owner merges after green CI and review. Safety-relevant
  PRs (WP-7, WP-10, WP-11, WP-12, WP-13) get an adversarial review and a
  recorded lab scenario before merge.

## Commands

```
make tools            # once per machine: the pinned linters
make lint             # gofmt, vet, staticcheck, golangci-lint; must print no issue
make race             # go test -race -shuffle=on ./...
make integration      # against real Postgres/Timescale/NATS (docker)
make generate         # oapi-codegen, sqlc, openapi-typescript
make check-generated  # generated code is up to date (CI runs this offline)
make check-schemas    # schemas/ examples validate and round-trip
make compose-up       # the seven processes, web, databases, NATS (profiles: demo, conformance)
make conformance      # InterUSS DSS + uss_qualifier against this USSP (WP-19)
make ci               # what CI runs, in order
```

## Before you say a work package is done

Run, in this order, and paste the last lines of each into the PR:
`make tools` (once), `make lint`, `make race`, `make integration`,
`make check-generated`, `make check-schemas`. Then check the brief's
done-when list item by item, including the lab scenario where the brief
names one, with its run recorded in `docs/RUNBOOKS/`. If a vector or a
scenario cannot pass without a behaviour the plan did not foresee, stop
and write it down in the PR as a spec gap; do not change the vector.
