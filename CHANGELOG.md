# Changelog

All notable changes to `uspace-ussp`. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow
semantic versioning, and the published API `api/openapi.yaml` changes
additively within `/v1`.

## [Unreleased]

### Changed

- WP-6: api projects policy, source_control, cis_current,
  registry_validity and client_bindings to NATS KV through the bounded
  `bus.Projector` instead of in-process stand-ins, so a write the KV
  cannot take is refused with 503 (B-09); the CIS cache retries a failed
  projection on its reconciliation tick (`cis_projection_retried`).
  `nats` on `/readyz` is degraded when a stream or bucket is missing or
  differs from this build's topology. Time-series migration 00003 adds
  `msg_id` with a unique `(msg_id, time)` index to every written
  hypertable, `telemetry.accuracy_h_m` / `accuracy_v_m`, a nullable `ts`
  (no source clock), and `writer_gaps` / `writer_positions`.

- WP-5: the api process has one outgoing token client for the CISP and
  the authority; a stored policy version without a threshold added
  later loads that threshold's default.
- WP-3: uspace-core v1.2.0; session tokens are signed by core's
  `Issuer.IssueSession` instead of a local signer; the token verifier
  builder moves to `internal/app/proc` (rid-sp verifies tokens too).

### Added

- WP-6 bus, partition cell and tsdb-writer: `internal/cell` over core
  `geodesy/cell` (Key, Ring1, CellsFor, CellsForEnvelope, ownership of
  `USSP_CELL_OWNERSHIP`; the 800 m ring guarantee checked against brute
  force on 10 000 points); `internal/bus` with the PLAN §7 streams (TRK,
  MAN, PEER, ALRT, CONF, IDENT, INTENT, CIS, TRAFFIC, INGEST) and KV
  buckets, created when missing and verified for drift, typed subject
  builders and parser, the 04 §2 envelope with ULID `msg_id`, the
  publisher (core for trk/man/peer/src/ctl, JetStream with the msg_id as
  dedupe id otherwise; `published_*`, `publish_failed_*`), the KV
  projector and `Follower[T]` (watch, ctl push, 300 s re-read, value with
  its age); `sources.Follow`; tsdb-writer: one durable pull consumer per
  stream, batched `COPY` (1000 rows or 1 s) through a staging table with
  `ON CONFLICT DO NOTHING`, ack after commit, a 10 s / 50 000-row queue
  (`USSP_WRITER_QUEUE_S`, `USSP_WRITER_HOLD_ROWS`), gaps for what the
  streams removed unwritten, malformed or refused (`writer_gaps`,
  `dropped_rows`), and queue depth, batch size and write latency on
  `/metrics`.

- WP-5 registry validity (`internal/registry`): the authority's F8 API
  generated from the pinned copy `api/clients/authority.yaml` (GET and
  POST `/v1/registry/validate` with `purpose` as the contract's query
  parameter, `GET /v1/registry/changes` with ETag; a `registry.validate`
  token for the authority's host, a 2 s deadline, a 1 MiB body cap);
  answers decoded strictly and checked against the keys asked, a field
  F8 does not define or an echoed secret part refused and counted
  `registry_pii_refused`; `registry.Cache` serving answers within the
  policy's `registry_positive_ttl_s` (24 h) and `registry_negative_ttl_s`
  (5 min, for `unknown`) with `cache_age_s`, an unanswerable key
  `unknown` with `registry_unavailable`, writes to `registry_validity`
  and the KV projection in one transaction (in memory until WP-6);
  `registry.Feed` polling the change feed every 30 s and invalidating by
  fold key with the cursor in `registry_feed`, a fetch that raced an
  invalidation not written (`registry_invalidations`, migration 00010);
  `registry.Lookup`, an `identify.Lookup` over our fleet and the cached
  answers whose Resolve methods answer `registry_unavailable` for a
  missing projection or a key without a fresh answer, and
  `Lookup.FleetInput` for WP-8; `GET /v1/registry/validate` for operator
  clients holding `ussp.intents`, recorded as `registry_validated` with
  the client and the purpose; operator accounts checked through the
  cache; `/readyz` entry `registry` (`last_success_age_s`, the feed's
  cursor and age); a fake authority in `internal/testfakes`.
- WP-4 CIS cache and geo-zone evaluation (`internal/cis`): the CISP's F3
  pull API generated from the pinned copy `api/clients/cisp.yaml`
  (conditional reads by ETag, versions, the change feed, an idempotent
  subscription with the callback `USSP_USS_BASE_URL` +
  `/v1/cis/notifications`); `POST /v1/cis/notifications` verifying the
  compact JWS of the CISP or, on its degraded direct path, the ANSP
  (core `CompactVerifier`, aud = this host, iat at most 5 min, delivery
  ids remembered in `cis_notification_jtis` on the database clock;
  `subscription_test`, `republished` and unknown reasons acknowledged
  without a pull; `pull_url` followed only over https on the CISP's
  scheme, host and port, a mismatch counted); the 60 s conditional reconciliation
  (`USSP_CIS_RECONCILE_S`); versions accepted whole through
  `ed318.Parse` and `ed318.ToZones` or refused whole with the previous
  kept and `/readyz` degraded; `cis_datasets`/`cis_features` (current
  and previous version) with a warm start; `cis.Evaluator`
  (`JudgePoint`, `AirspacesAt`, `ZonesFor`, `JudgeHeightLimit`, `Age`
  with the policy's `cis_stale_s`) over core's zones and `ed318.Applies`;
  the per-cell `zone/applicable/v1` projection for KV `cis_current`
  (in memory until WP-6); `/readyz` entries `cis` (versions and age, or
  unknown, stale, refused, not pulled) and `cis_notify_keys`; fakes of
  the CISP and the ANSP in `internal/testfakes`. An up dependency now
  keeps the detail its probe gives.
- WP-4 provenance: a CIS version is used only when its publisher's
  `X-Publisher-Signature` (read from `GET /v1/{dataset}/versions/{v}`)
  verifies with the authority's or the ANSP's keys
  (`USSP_CIS_PUBLISHER_KEYS`, iat bound
  `USSP_CIS_PUBLISHER_SIG_MAX_AGE_S`, default 366 days); otherwise it is
  held (stored with `signature_ok` false, never used or warm-loaded),
  `/readyz` `cis` degraded and `cis_publisher_untrusted` counted;
  `/readyz` entry `cis_publisher_keys`.

- WP-3 standards code generation: the F3411-22a and F3548-21 OpenAPI
  files pinned byte for byte in `api/standards/` with `SOURCE` (the
  commits and SHA-256 uspace-core v1.2.0 records; `check-standards.sh`
  offline, `fetch-standards.sh` with network); `internal/stdapi/f3411`
  and `f3548` generated by oapi-codegen v2.8.0 (strict servers of the
  USS-side operations, `StdClient` of the DSS and peer USS operations)
  on core's wire types, no struct generated twice;
  `internal/stdapi/convert`, the one wire boundary, through core's
  validating `Unmarshal*` and `Volume4DToZonesEnvelope`; api mounts the
  F3548 USS endpoints and rid-sp the F3411 ones behind the file's scopes,
  each 501 `not_implemented` after the guard until WP-9 and WP-13;
  `httpx.Access.AllScopes` for a requirement of several scopes;
  `check-generated.sh` regenerates and diffs the four files.
- WP-2 auth and accounts: `internal/auth` on uspace-core's verifier
  (own issuer and allow-listed ecosystem issuers, `USSP_AUDIENCES`,
  `StrictSessionClaims`; the ecosystem JWKS fetched in the background
  so api starts while the token service is down, `/readyz` `jwks` up,
  degraded with the cache's age, or down), the guard behind a
  fail-closed access table (every operation of `api/openapi.yaml` has
  an entry or api refuses to start; refusals typed by core's counter,
  counted and audited with the token's `sub` or `unknown`), this
  USSP's issuer (`USSP_ISSUER_KEY_FILE`, `kid` = RFC 7638 thumbprint,
  the previous key published during a rotation,
  `scripts/gen-issuer-key.sh`), `POST /oauth/token` (client
  credentials, Basic or body, argon2id, previous secret within
  `policy.client_secret_overlap_s`, per-client and per-address limits,
  `policy.operator_token_ttl_s`), `GET /.well-known/jwks.json`, session
  JWTs (M20) with the `uspace_session`/`uspace_csrf` cookies and the
  CSRF double submit (M21), the WebSocket upgrade by cookie and
  `Origin` with 4401 (M22), the outgoing token client, the
  `client_bindings` projection (in memory until WP-6), the client
  address taken only from `USSP_TRUSTED_PROXIES`; `internal/accounts`
  and `/v1/accounts/*` (operator self-registration pending until the
  registry says valid, clients with secrets shown once and rotation,
  serial bindings through `core/serial`, staff accounts with TOTP for
  `admin`, `ussp-api staff-add`, sign-in with a per-username lockout
  and session rows in the database so both hold across replicas,
  logout, `/me`); migration 00008 (`portal_users`, `sessions`,
  `login_lockouts`, the rotation columns, one live client per serial).
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
