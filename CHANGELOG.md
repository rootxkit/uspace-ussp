# Changelog

All notable changes to `uspace-ussp`. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow
semantic versioning, and the published API `api/openapi.yaml` changes
additively within `/v1`.

## [Unreleased]

### Added

- WP-1 store and migrations: the two goose trees, embedded and never
  merged (`migrations/relational`: every table of PLAN §5.1 with PostGIS
  geography, the indexes of the brief and monthly `events` partitions;
  `migrations/timeseries`: the seven hypertables of §5.2 with 1-day
  chunks, compression after 7 days and 90-day retention, `peer_flights`
  in 1-hour chunks dropped after 24 h), version tables
  `goose_db_version_relational` / `goose_db_version_timeseries`; the
  application role `ussp_app` with SELECT and INSERT only on `events`,
  `intent_versions` and `conformance_states`; `ussp-api migrate` and
  `ussp-tsdb-writer migrate` (`up`, `down [version]`, `status`, advisory
  lock), the only code path that migrates, with `scripts/migrate.sh`,
  `make migrate-up/down/status` and a second one-shot compose service;
  processes wait up to `USSP_SCHEMA_WAIT_S` for their schema and refuse
  an older one, naming both versions, and `/readyz` reports an older
  schema; `internal/store` (pools, sqlc queries, `Tx`, the outbox, the
  `Audit` writer); `internal/policy` (versioned `Values` with the Q6
  defaults, `Load`, `Put` refused with a 503-shaped error when the KV
  projection fails, `Current`); `internal/sources` (`Switch`,
  `Republish`, `List` on uspace-core sources, with version, epoch and
  the same refusal).
- WP-0 scaffold: the Go module pinned to `uspace-core` v1.1.0; the seven
  processes (`api`, `telemetry-ingest`, `rid-sp`, `monitor`,
  `traffic-ws`, `dss-sync`, `tsdb-writer`) and `ussp-dev`, each serving
  `/healthz`, `/readyz` (every dependency with state, since, age and
  reason; 503 while a required one is down) and `/metrics`
  (`ussp_build_info`, `ussp_dependency_up`, `ussp_dependency_age_s`),
  starting degraded when PostgreSQL, TimescaleDB or NATS is down and
  draining on SIGTERM; the `USSP_*` configuration (`deploy/ENV.md`) with
  the token verifier configuration (`StrictSessionClaims` on); `httpx`
  (timeouts, body caps, request ids, recovery, problem+json, the scope
  middleware); the OpenAPI 3.1 skeleton with the generated server and
  client; the Makefile, CI, the two images and the compose stack; the
  web shell (Next.js, `ka`/`en`, generated API types).
