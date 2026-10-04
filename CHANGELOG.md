# Changelog

All notable changes to `uspace-ussp`. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow
semantic versioning, and the published API `api/openapi.yaml` changes
additively within `/v1`.

## [Unreleased]

### Fixed

- This USSP's client id at the authority keeps the case of its code
  (`ussp-ABC1-01`, not `ussp-abc1-01`). The authority's token service
  registers only `ussp-<code>-<nn>` with an upper-case code (M8), and the
  lab issuer lists `ussp-USSP-DEV-01`, so the lower-cased id could never
  be registered and every outgoing call (the CIS) had no token. Found at
  the first staging deploy: `POST /v1/oauth/clients` refused it.

### Added

- WP-13: F3548 strategic coordination through the InterUSS DSS and with
  the peer USSPs (`internal/dss`, running in api, D5). An intent that
  needs an authorisation inside U-space airspace (anywhere with the new
  `USSP_DSS_FOR_ALL=on`) is no longer authorised on the local checks: it
  is `pending_dss` (`dss_write_pending`, `dss_unavailable`,
  `uss_availability_down`, `dss_key_conflict`, `peer_intent_unavailable`
  or `dss_refused`) until the DSS has taken it, and holds its place in
  the first come, first served order meanwhile. Every version of an
  intent the DSS must hold queues an `oir_put` outbox item in the
  transaction that wrote it; the writer (per-intent advisory lock) reads
  the operational intent and constraint references of the extents,
  fetches the peers' details and the constraints from their managers
  (stored in `peer_intents` and `constraints`, trust provider), judges
  the intent again with WP-7's deconfliction and the CIS as it is now
  (`intent.PeerCheck`: a peer first or a constraint rejects it, naming
  `peer:<id>` or the constraint), PUTs it Accepted with the key of every
  ovn seen and an implicit subscription, fetches what a 409 names and
  writes once more, then authorises it (its number, the previous one
  after a modification) and tells the subscribers through `peer_notify`
  items within 5 s; a peer it displaces is told inline within 900 ms,
  else `peer_notify_late`. Activation, nonconformance, contingency and
  the end are mirrored (PUT with the state, DELETE at the ovn), the
  state written only ever one of the four F3548 states. `POST
  /v1/intents` and a modification make the write in the request path
  (at most 5 s) and answer the decision as it then stands. The F3548 USS
  endpoints replace WP-3's 501s: our details as the DSS holds them, the
  telemetry of a Nonconforming or Contingent intent, peer notifications
  (stored by version, published on the new core subject
  `peer.intent.v1.<entity_id>` as the new schema `peer/intent/v1`, judged
  against our intents: a peer with precedence withdraws or marks ours, a
  conflict the DSS let through is audited `dss_conflict_reported` for the
  authority), constraint notifications (stored, WP-12's re-check), reports,
  the log sets of an exchange log (`dss_exchanges`) and 404 for our
  constraints. One DSS subscription per U-space airspace of
  `cis_current`, renewed at 80 % of 24 h; this USSP's availability read
  every 60 s (`Down` stops new writes; `ctl.dss_state`); peer data purged
  at 24 h unless a decision names it. `/readyz` `dss` merges the F3411
  and F3548 parts (reachability, availability, outbox depth and age, the
  DSS clock drift beyond 5 s). Relational migration 00017; policy values
  `peer_subscription_margin_m` (2000) and `dss_exchange_retention_days`
  (7); the F3548 client gains `getConstraintDetails` and
  `getSubscription`; the fake DSS gains the F3548 side with ovn and key
  semantics; `internal/testfakes/peeruss` is a fake peer USSP.
- Every NATS stream's age and size bound and every bucket's size bound
  is configurable: `USSP_<STREAM>_STREAM_MAX_AGE_S` and
  `USSP_<STREAM>_STREAM_MAX_BYTES` for TRK, MAN, PEER, ALRT, IDENT,
  INTENT, CIS, TRAFFIC, INGEST and FLIGHT (CONF already was), and
  `USSP_<BUCKET>_BUCKET_MAX_BYTES` for every bucket, each defaulting to
  the bound it had. A deployment with a smaller JetStream file store than
  the defaults' 12.6 GiB lowers them to fit; every process creates a
  missing stream or bucket with the configured bounds, not the defaults.
  Bucket TTLs stay fixed. `TestTopologyFitsTheFileStore` (now in
  `internal/app/proc`) checks the bounds the environment configures
  against `USSP_TEST_NATS_MAX_FILE_STORE`, or the compose store.
- WP-12: geo-awareness, zone alerts and the standing re-check. api
  serves `GET /v1/geo?bbox=&at=` and `GET /v1/geo/intents/{id}` (scope
  `ussp.geo`) from its CIS cache: U-space airspaces with their Art. 3(4)
  requirements, zones with their ED-318 feature verbatim and their
  limits, restrictions with state and window, each with `updated_at`,
  `version` and `valid_from/to`, and `cis_version`, `cis_age_s`,
  `stale`. Every installed CIS version is published on
  `cis.v1.<dataset>` as the new schema `geo/changed/v1`, which traffic-ws
  forwards to every traffic subscription. The monitor judges this USSP's
  flights against the zones of `cis_current` with uspace-core's
  `alerting.Monitor` (conflicts skipped), rebuilt within one tick of a
  new projection, and publishes `alert/v1` `zone_incursion`,
  `identification` and `identification_mismatch`; active zone alerts are
  carried across a rebuild, a restart and a handover in each flight's
  `conformance_state`. The standing re-check (Art. 10(10), PLAN §15.1
  Q20) runs on every installed version of zones, uspace_airspace and
  restrictions and every 60 s: an accepted intent that now conflicts is
  withdrawn, an activated one is marked, and the operator gets
  `restriction_activated` (critical, `flight_id` null before a flight)
  and the decision's `change_reason`. `alert/v1` gains the two
  identification kinds, a nullable `flight_id` for
  `restriction_activated`, and the `zone_incursion` and
  `restriction_activated` details. Policy values `zone_clear_after_s`
  (3), `zone_stale_after_s` (15), `zone_conditional_severity`
  (warning). `USSP_TERRAIN_DIR` is read by the monitor. A decision is
  granted only on the CIS version still current at its commit (else it
  is assessed again, at most three times, then 503 `cis_changed`), and
  an activation is judged on the CIS as it is now (409
  `authorisation_withdrawn` on a conflict). A second conflict while the
  first still applies is a new notice; a displacement whose re-check
  never ran is recovered by the sweep; api republishes open
  `restriction_activated` notices every 10 s, and they never mark the
  monitor degraded in traffic-ws. A carried zone alert whose zone did not
  build, or whose set is over its bound, is kept.

- WP-11: traffic information and the CPA proximity alert. The monitor
  feeds one uspace-core `alerting.Monitor` per owned cell set with every
  `trk.v1`, `peer.v1` and `man.v1` sample of its cells and their ring-1
  (`internal/traffic`) and publishes `alert/v1` `proximity` for each of
  this USSP's flights in a pair, naming the other aircraft and its trust;
  active alerts persist in the new KV bucket `proximity_state` and are
  carried across a restart, a handover and a policy change (PLAN §15
  Q21). traffic-ws serves `WS /v1/traffic`, `GET /v1/traffic/snapshot`
  and `WS /v1/alerts` in the console frame (M29). api records every
  `alert/v1` (relational migration 00016: `alerts.cell5`, `recorded_at`,
  state checks, the escalation index), serves `POST
  /v1/alerts/{alert_id}/ack` and escalates critical alerts left
  unacknowledged. New schemas `alert/v1` and `traffic/product/v1`; pinned
  copies of `console/snapshot/v1`, `console/subscribe/v1` (uspace-lab)
  and `track/manned/v1` (uspace-ansp). Policy values
  `cpa_clear_after_s` (3), `cpa_stale_after_s` (15),
  `cpa_pair_budget_count` (50 000), `traffic_radius_m` (2000),
  `traffic_live_max_age_s` (2), `traffic_stale_after_s` (5),
  `traffic_drop_after_s` (60), `traffic_throttle_track_count` (200),
  `traffic_record_every_s` (10), `escalation_after_s` (30).

### Changed

- Retro-audit fixes (Fable audit of WP-3, WP-5, WP-6, and the CI and
  system findings). Every stream and bucket has a `max_bytes`, together
  within the compose file store, now 16 GB (existing streams report the
  new bounds as drift on `/readyz`); a new bucket `sessions_live`, which
  api writes in the session transaction and traffic-ws reads, so a
  session signed out, ended or idle in api is refused there and its
  sockets close with 4401. The CIS cache installs a version only when
  its features are the ones its publisher signed. The receiver refuses
  to start unless `USSP_AUDIENCES` holds the host of
  `USSP_USS_BASE_URL`. `GET /v1/registry/validate` answers the caller's
  own keys in full and every other key, and every pilot, status only,
  limited per client by `USSP_REGISTRY_RATE_PER_MIN` (60). The registry
  feed deletes projected answers the table has no row for, and a lookup
  error never carries its URL. tsdb-writer records a `position_unknown`
  gap (timeseries migration 00004), keeps held messages alive while
  writes succeed, coalesces malformed and rejected gaps and their log
  lines, and never acknowledges a rejection whose gap was refused. api
  reports `client_address` degraded while an untrusted peer sends
  `X-Forwarded-For`. Counters `captured_trk`, `captured_man`,
  `captured_peer`, `hole_undercounted`, `registry_feed_orphans_deleted`,
  `registry_lookup_cancelled`, `xff_from_untrusted_peer`. CI pins every
  action to a commit SHA, builds pull-request images without write
  permissions, runs gitleaks from a checksummed binary and runs the Go
  jobs for testdata, schemas and fixtures.
- WP-11: ALRT is bounded at 1 GiB and TRAFFIC at 512 MiB (existing
  streams report the drift on `/readyz` until an operator updates them).
  tsdb-writer reads the `traffic/product/v1` record sample (`for`,
  `tracks`, `degraded[].input`). `make integration` allows the suite 30
  minutes (CI job 25). The OpenAPI's traffic tag is served by traffic-ws
  from `internal/traffic/gen`. `bus.Replay.OpenSubjects`,
  `bus.Cell3Filter`, `monitor.Subjects`.

- WP-10: policy values `conformance_clear_after_s` (3 s),
  `pressure_uncertainty_m` (250 m) and `monitor_live_max_age_s` (10 s),
  core's alerting and zones defaults. `deconflict.PointDistanceM` and
  `intent.WireVolume` are exported so conformance reads an outline as
  deconfliction does; `intent.Service.SetConformance` moves an activated
  intent to nonconforming, contingent and back (event
  `intent_conformance`). `bus.Conn.Listen` is a core subscription for
  the processes. The relational queries `InsertConformanceState` and
  `ConformanceTimeline`. The monitor process now has routes and workers.
  The monitor persists each flight's conformance state machine in the
  new KV bucket `conformance_state` (written by monitor) and restores it
  at a start and on a cell handover between instances; a fresh tracker
  takes its intent's nonconforming or contingent state, and api moves an
  intent back to activated only on the tracker's own return transition.
  CONF keeps 48 h and 4 GiB (`USSP_CONF_STREAM_MAX_AGE_S`,
  `USSP_CONF_STREAM_MAX_BYTES`) and carries the transitions and a 0.1 Hz
  heartbeat per flight; `bus.KVStore` gains revision writes.

- WP-9: `flight/event/v1` carries an optional `position` (the flight's
  newest live position, its first on `started`); `intent/state/v1`
  carries the optional `category`, `class_label` and `ua_registration`
  (Annex IV items 4 and 10). api records each flight fact and its F3411
  ISA plan in one transaction and lists `dss` on `/readyz`; relational
  migration 00013 adds `dss_isas.kind`. The KV bucket
  `rid_isa_notifications` joins the topology (written by rid-sp). The
  integration helper `withAuth` points api at a fake DSS.

- WP-8: uspace-core v1.3.0. `httpx.Access.WebSocket` (an upgrade that
  authenticates itself, M22) and `auth.WSAuth.Admit` (a 503 with
  Retry-After before the upgrade, B-10); `bus.Mirror` (every key of a KV
  bucket with its age), `bus.StreamSource.NeverDelivered`, the FLIGHT
  stream and `flight.v1.<event>.<flight_id>`; `registry.Keys`;
  `cis.FeatureZones`; telemetry-ingest reads `USSP_ISSUER_URL`. The api
  server is generated without the telemetry tag, which telemetry-ingest
  serves from its own generated interface (`internal/telemetry/gen`).

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

- WP-10: conformance monitoring (Art. 13(1)). `internal/conformance`:
  `Judge` (a sample against its authorised volumes, AMSL bands, window
  and deviation thresholds; undetermined without a vertical position,
  pressure judged with its margin and `within_band`), `Tracker`
  (admission as core's alerting; conforming, nonconforming after t_s or
  at once beyond a threshold, back after the hysteresis, contingent
  after F3548's 60 s, lost_link after `lost_link_s`, unknown with its
  reason), the `nonconformance` and `lost_link` alerts (critical) and
  the `nonconformance_nearby` fan-out (warning), `conformance/state/v1`
  on `conf.v1.<flight_id>` and `alert/v1` on `alrt.v1`, and the
  `Recorder` api runs on `CONF` (durable `api-conformance`) to append
  `conformance_states` and move the intent. The monitor process (one
  worker per home cell3, 1 s tick, republish of every active alert,
  `/readyz` entries `trk`, `intent_active`, `policy`, `source_control`,
  a conformance status line every 10 s). `testdata/vectors/conformance.json`
  (68 cases) with its generator, fuzz targets for the track, state and
  intent_active readers, `BenchmarkConformanceJudge`. Measured in the
  integration suite: nonconformance 2.3 ms after the sample's
  `captured_at`; the S-M2 lab SITL run is owed (`docs/RUNBOOKS/WP-10.md`).

- WP-9: the ASTM F3411-22a network identification Service Provider.
  rid-sp keeps our own flights (authenticated, operator_ws) for 60 s in
  memory, replayed from `TRK` at start (`internal/ridsp.Window`, at most
  `rid_recent_positions_max_count` samples a flight), and serves `GET
  /uss/flights` (`rid.display_provider`; the standard's view, 413 with
  the standard's ErrorResponse above 7 km, every flight with a position
  of the last 60 s in the view, Table 1's special values through core's
  constants, `recent_positions` up to 60 s, each flight checked by core
  before it is sent) and `GET /uss/flights/{id}/details` (uas_id with
  serial, UA registration and our flight id, the public part of the
  operator registration, the remote pilot's position, the authorisation
  number, the Annex IV EU classification). `POST
  /uss/identification_service_areas/{id}` (`rid.service_provider`) keeps
  a peer's ISA notification in `rid_isa_notifications` (400, 403 another
  sender, 409 same version another entity). The ISA of every flight:
  planned by api with the flight's facts (the intent's volumes as a
  box with their W84 band and window plus 60 s, or a session ISA on a
  fixed grid around the first position, renewed while it flies) and
  written by `ridsp.ISAWorker` through `dss_outbox` with backoff (PUT,
  409 recovered by reading the version, DELETE with the version), every
  subscriber the DSS lists notified with `aud` = its host; DSS down:
  `/uss/flights` serves, `dss: down since T` with the waiting writes.
  The optional `WS /v1/authority/flights` (`USSP_AUTHORITY_PUSH=on`,
  404 when off): 1 Hz `authority/flight/v1` frames (a new owned schema),
  a ten-minute buffer of at most 120 000 frames whose shed frames are a
  counted gap on the next status frame. `internal/dss/fakedss` (the
  F3411 DSS for tests), policy values `rid_recent_positions_max_count`,
  `session_isa_radius_m`, `session_isa_horizon_s`, the histogram
  `ussp_rid_sp_flights_seconds`. Measured in the integration suite:
  100 flights at 1 Hz, 10 views at 1 Hz for 60 s, p95 38 ms and p99
  46 ms alone (186 ms and 339 ms inside the full suite), nothing older
  than 60 s; an ISA in the DSS 2 s after a flight starts and 8 s after
  a 30 s DSS outage ends.

- WP-8: telemetry ingest and flights. `WS /v1/telemetry` (bearer
  `ussp.telemetry`, the console frame: `telemetry/v1` bodies in,
  `console/status/v1` frames out and nothing else, `acked_seq` per
  serial) and `POST /v1/telemetry/batch` (at most 1 s of samples per
  serial, placed against `sent_at`, 202 only for what reached the bus).
  Bound serials only (`refused_unbound`), one socket per aircraft
  (`refused_replaced`, B-14), 2 Hz live and 20 Hz backlog per aircraft,
  replays acknowledged and published once, a disabled source refused
  with 503 and Retry-After and closed with 1013 (B-10), a teleport above
  100 m/s flagged `anomaly: teleport` (T3). Time placement through
  uspace-core (`timeplace.PlaceNetwork` for a sample against its
  `sent_at` or our clock, `PlaceBatch` for a batch without one) with the
  aircraft's anchor so input read late is placed by its own time (T-11,
  SC-15), `backlog` from the client or from the placement (T-04), order
  per live and backlog stream (T-03, T-13); AMSL through the geoid and
  `rid.AltitudeSelector` (R-07, R-08; `geoid: down, missing` on
  `/readyz`); identification by `identify.ResolveBound` over the
  registry projection, changes on `ident.v1`; inside U-space airspace an
  activated intent of the aircraft or `refused_no_authorisation` /
  `refused_intent_state`. Tracks on `trk.v1` (`track/telemetry/v1`,
  trust authenticated, source operator_ws); the publisher's bounded
  memory spills to the `ingest.v1.<cell3>` work queue, whose drain
  replays as backlog and sheds the oldest with a gap record on `src.v1`
  (also what the queue removed unread); every client's
  `source/status/v1` on `src.v1.operator_ws.<client>` every 2 s.
  `internal/flights`: the flight binder (`started`, `telemetry_lost`,
  `telemetry_resumed`, `ended` on `flight.v1`) and api's recorder of the
  flights table. Schemas `telemetry/v1`, `ident/change/v1`,
  `flight/event/v1` (owned) and pinned copies of the lab's `envelope/v1`,
  `track/telemetry/v1`, `source/status/v1`, `console/status/v1`
  (`schemas/CONSUMED`); policy values `backlog_after_s`,
  `flight_end_after_s`, `ingest_queue_s`, `ingest_backlog_max_s`,
  `telemetry_rate_hz`, `telemetry_backlog_rate_hz`,
  `telemetry_dedupe_s`, `telemetry_ahead_tolerance_s`,
  `telemetry_anchor_max_age_s`, `telemetry_batch_span_s`,
  `pressure_fallback_accuracy_code`, `pressure_hold_s`,
  `teleport_speed_ms`; the simulated operator client
  `internal/testfakes/operator`. Measured in the integration suite: 100
  samples/s for 60 s, 6000 of 6000 rows; after a 60 s outage of 20
  aircraft the drain ran at 599 samples/s, 29.9x the intake.

- WP-7: flight authorisation. `POST`, `GET`, list and `PATCH
  /v1/intents` (scope `ussp.intents`, the operator's own intents only):
  the ten Annex IV items validated (problems name `annex_iv.N`), volumes
  through uspace-core's F3548 validation and the geoid to AMSL, then the
  registry (F8, purpose authorisation), the CIS cache (stale or known
  outdated refuses; U-space airspace and its ceiling; zones; ANSP
  restrictions), strategic deconfliction (`internal/intent/deconflict`,
  priority then first come first served), the DSS, the deviation
  thresholds and the authorisation number. A missing, stale or untrusted
  input refuses or holds; it never authorises. States, versions, the
  time_end sweep, KV `intent_active` and `intent.v1.<state>.<id>`
  written after the commit (an intent the bus did not take is
  republished by the sweep; relational migration 00012).
  Schemas `intent/request/v1`, `intent/decision/v1`, `intent/state/v1`
  with examples both ways and `scripts/check-schemas.sh` validating them;
  `testdata/vectors/deconfliction.json` (74 cases). Relational migration
  00011; policy thresholds `special_operation_priority`,
  `deconflict_buffer_m`, `deconflict_vertical_buffer_m`,
  `activation_lead_s`, `intent_open_max_count`; `cis.Cache.Outdated`;
  api reads `USSP_GEOID_FILE` and lists `geoid` on `/readyz`.

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
