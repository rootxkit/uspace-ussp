# WP-1: store and migrations

Branch `feat/WP-1-store-migrations`. Milestone S-M0. Owns
`migrations/relational/`, `migrations/timeseries/`, `internal/store/`
(pools, sqlc config and generated queries, transactions, outbox helper),
`internal/policy/`, `internal/sources/` (the writer side; the follower is
WP-6's), the `events` audit writer. Depends on WP-0. Consumers: every
other WP.

## Read first

1. `CLAUDE.md`, `docs/PLAN.md §2` D5, D6, D10, `§5` (every table), `§8`
   (audit grants), `§15` Q3, Q18.
2. Spec `03` preamble and `§3` (ussp tables), `05 §4` (retention and
   compression), `04 §3.6` (policy and source control messages), `06 §2`
   T7.
3. LESSONS INV-03, B-09 (switch written inside the transaction, refused
   with 503 when the KV cannot take it), B-15 (two trees, never merged),
   G-08 (projection written with the change), E-10.
4. `uspace-core/sources` (`State`, `Control`, `Follower.Apply` semantics:
   version strictly higher within an epoch, any under a new epoch) and
   `core.Counters`.
5. Reference only: utm `infra/migrations/relational` and `telemetry`
   (two trees), `docs/runbooks/u15-source-control.md`.

## What to build

- `migrations/relational/0001_*.sql ...` with goose, embedded (`embed.FS`),
  version table `goose_db_version_relational`: every table of PLAN §5.1
  with PostGIS geography columns, indexes (`operational_intents`:
  `envelope_geom` GiST, `(time_start, time_end)`, `(operator_id,
  created_at)`, unique `(client_id, client_ref)`; `alerts`: `(flight_id,
  raised_at)`, partial on `cleared_at IS NULL`; `cis_features`: GiST,
  `(dataset, version)`; `registry_validity`: `(entity_type, key)`;
  `events`: monthly partitions by `ts`), the application role `ussp_app`
  with no `UPDATE`/`DELETE` on `events`, `conformance_states`,
  `intent_versions`, and a migration test that proves the grant refuses
  (E-01: and that an `INSERT` works).
- `migrations/timeseries/0001_*.sql ...`, version table
  `goose_db_version_timeseries`: the hypertables of PLAN §5.2 with
  `create_hypertable` (1-day chunks), compression policies (after 7 days,
  `segmentby`, `orderby`), retention policies (`peer_flights` **24 h**;
  `telemetry` from policy, default 90 days; the rest 90 days), PostGIS
  geometry columns. A test inserts a `peer_flights` row 25 h old, runs
  the retention job, and finds it gone, and one 23 h old and finds it
  kept (E-01).
- `internal/store`: `Open(ctx, cfg) (*Store, error)` with two `pgxpool`
  pools (`Rel`, `TS`), `Migrate(ctx, tree)` with an advisory lock and
  `WaitForVersion(ctx, tree, n)` for non-owner processes, `sqlc` configs
  (`sqlc.yaml`: one package per tree, `pgx/v5`, emit JSON tags with the
  wire names), `queries/*.sql` for every access this plan names
  (list them per package in the PR), `Tx(ctx, fn)` helper, an `Outbox`
  helper (`Enqueue` inside a tx, `Claim(n)`, `Done`, `Fail(backoff)`),
  and the audit writer `Audit(ctx, tx, Event)` that every mutating path
  calls (it is the only writer of `events`).
- `internal/policy`: `Values` struct with units in names and the defaults
  of PLAN §15 Q6 plus core's CPA defaults; `Load(ctx)` (newest row),
  `Put(ctx, actor, reason, Values)` (new version in a tx, audit, KV
  projection write through an interface `Projector` WP-6 implements; a
  failed projection refuses the write, B-09), `Current()` for `api`.
- `internal/sources` (writer): `Switch(ctx, actor, reason, Control)`:
  advisory lock, DB sequence version, epoch from a `source_control_epoch`
  table row created at migration time, KV write inside the tx hook, 503
  on KV failure; `Republish(ctx)` every 60 s from the database (B-09
  repair); `List(ctx)`.
- The `migrate` subcommand on `cmd/api` (relational tree) and
  `cmd/tsdb-writer` (timeseries tree): `up`, `down`, `status`, with the
  advisory lock; the only code path that runs a migration
  (reconciliation M36). Process start never migrates: `WaitForVersion`
  refuses to start on a lower version, printing which. `scripts/migrate.sh`
  wraps the subcommands for a URL; `make migrate-up/down`.

## Done when

- [ ] Both trees `up`, `down`, `up` against the CI service containers;
  `goose status` shows separate version tables; a migration file placed
  in the wrong tree is caught by a test that parses each tree's files
  for the other's table names.
- [ ] sqlc generated code committed and `check-generated.sh` covers it.
- [ ] Grants proven both ways; retention proven both ways; the policy
  write with a failing projector returns the 503-shaped error and leaves
  no new row (E-01 twin: a succeeding projector leaves one row and one
  audit event).
- [ ] `WaitForVersion` test: a process started against an older schema
  waits, then proceeds when the `migrate` subcommand lands it (run from
  the test, not from the process); a lower version after timeout
  returns an error naming the versions; a grep of `cmd/*` proves no
  process calls the migrator outside the `migrate` subcommand.
- [ ] `-race -shuffle=on` green; lint clean; `CHANGELOG.md` line; PR with
  outputs.

## Safety notes

- Column names carry units and datums (`alt_amsl_m`, `alt_wgs84_m`); a
  column named `alt` or `height` without reference is a review failure.
- No AGL column anywhere (D-02). `height_m` + `height_ref` is the
  broadcast's own field, never a judged AGL.
- The 24 h retention on peer data is a standard limit, not a tuning knob:
  it is not in `policy`.

## Commits

`feat(db): relational schema for intents, flights, alerts, dss and cis cache [WP-1 S-M0]`,
`feat(db): timeseries hypertables with compression and retention [WP-1 S-M0]`,
`feat(store): pools, migrations, sqlc queries, outbox and audit [WP-1 S-M0]`,
`feat(policy): versioned policy row with projection [WP-1 S-M0]`,
`feat(sources): source switches with version, epoch and refusal [WP-1 S-M0]`.
