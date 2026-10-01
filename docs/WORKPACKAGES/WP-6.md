# WP-6: bus, partition cell and the time-series writer

Branch `feat/WP-6-bus-cell-tsdb`. Milestone S-M0. Owns `internal/bus/`,
`internal/cell/`, `internal/app/tsdb-writer` and `cmd/tsdb-writer`, the
stream, subject and KV definitions of PLAN §7, the `Projector`
implementation that WP-1/2/4/5 wrote against, and the follower side of
`internal/sources`. Depends on WP-1. Consumers: every process.

## Read first

1. `docs/PLAN.md §2` D6, D7, `§3.2`, `§7` in full, `§9`, `§15` Q2, Q15.
2. Spec `05 §2`, `§3` (subjects, consumer scaling, cell ownership map),
   `§5` (backpressure table), `§6` (failure domains: NATS down, TimescaleDB
   down), `04 §2` (envelope), `04 §3.6`.
3. `uspace-core/sources` (`Follower`), `core.Counters`, `core.Times`.
4. LESSONS B-05 (persist before ack, dedupe replays), B-06 (no lock
   across I/O), B-07 (bounded, counted buffer while storage is down),
   B-08 (reconnect forever; start degraded), B-09, B-16, E-10, SC-08 step
   8, SC-18.
5. Reference only: utm `gateway/` batching writer (P1-13) and
   `common/source_control.py` follower.

## What to build

- `bus.Conn`: `nats.go` with JetStream, per-process credentials file,
  reconnect forever (`MaxReconnects(-1)`), a bounded initial connect (3
  tries with backoff, then start with `nats: down` on `/readyz` and keep
  retrying in the background: the console must load with an empty bus,
  E-02).
- `bus.Streams`: idempotent creation of `TRK` (mirror of `trk.v1.>`, 1 h),
  `ALRT` (7 d), `CONF` (30 d), `IDENT` (24 h), `INTENT` (30 d), `CIS`
  (30 d), `TRAFFIC` (1 d), `INGEST` (work queue, 10 min, `trk`-sized max
  bytes) with the subject filters of PLAN §7; KV buckets `cis_current`,
  `policy`, `source_control`, `registry_validity`, `client_bindings`,
  `intent_active` with history 1 and no TTL except `registry_validity`.
  A `Verify()` that compares what exists with what is expected and
  reports drift on `/readyz`.
- `bus.Subjects`: typed builders and parsers for every subject (a wrong
  token count is an error, never a panic); the envelope of `04 §2`
  (`schema`, `msg_id` ULID, `producer`, `ts`, `rx_ts`, `captured_at`,
  `time_source`, `backlog`) as a struct every message embeds, with
  `core.Times` conversion.
- `bus.Publisher`: core publish for `trk`/`man`/`peer`/`src`, JetStream
  publish with dedupe id (`msg_id`) for the durable subjects, counters
  `published_*`, `publish_failed_*`.
- `bus.Projector`: implements the `Projector` interface of WP-1/2/4/5
  (`Put(bucket, key, value)` inside a tx hook; returns an error that the
  writer turns into 503 when the KV is unreachable).
- `bus.Follower[T]`: watch a bucket, keep the last value, re-read every
  300 s, expose `Value() (T, ageS float64, ok bool)`; `sources.Follower`
  from core wrapped with the `ctl.sources` push.
- `cell`: a thin wrapper over `uspace-core/geodesy/cell` (additive in
  core v1.1.0, core WP-14; reconciliation M35): `Key(core.LatLon)
  (cell5, cell3 string)`, `Ring1(cell5) []string`, `CellsFor(geodesy.BBox)
  []string`, `CellsForEnvelope` (intents), `Ownership` parsed from
  `USSP_CELL_OWNERSHIP` (`all` or a list of `cell3`) with `Owns(cell3)
  bool`. Grid as core defines it: `cell5` = 0.1° × 0.1°, `cell3` = 1° ×
  1°, names `c5:<lat_idx>:<lon_idx>` / `c3:<lat_idx>:<lon_idx>`; the
  same grid the authority uses. If core v1.1.0 is not tagged when this
  WP starts, implement the identical grid locally behind the same
  function names and replace it with the import in a `build(deps)`
  commit; never a second grid definition. Property tests: every point
  maps to one cell; neighbours of a cell contain every point within
  800 m of any point in it (the CPA ring guarantee, C-15; at 0.1° a
  ring-1 margin is ≥ 7 km everywhere in Georgia).
- `tsdb-writer`: JetStream pull consumers on `TRK` (mirror), `man`,
  `peer`, `TRAFFIC`, `CONF` → batched `COPY` into the hypertables with
  `pgx.CopyFrom`, batches of ≤ 1000 rows or 1 s, ack after commit (B-05),
  dedupe by `msg_id` within a 10 s window; a bounded in-memory queue of
  10 s (`policy.WriterQueueS`); beyond it, stop acking so the stream holds
  the rest (the hot path never blocks: it publishes to core NATS and the
  mirror); when TimescaleDB is down, hold up to 50 000 rows (B-07), drop
  oldest counted, resume from the stream on recovery. Metrics: queue
  depth, batch size, write latency, `dropped_rows`, `dedupe_hits`.

## Done when

- [ ] Integration: 1000 `trk` messages published → 1000 telemetry rows;
  the same batch republished → 0 new rows, `dedupe_hits` = 1000 (B-05
  both ways).
- [ ] TimescaleDB stopped for 30 s under 100 msg/s → nothing lost, rows
  land after recovery, queue depth metric read and printed (SC-18); over
  the cap → `dropped_rows` moves and the log says so.
- [ ] NATS unreachable at start → process starts, `/readyz` says `nats:
  down`, reconnects when it appears (E-02, SC-08 step 8).
- [ ] KV missing at follower start → `Value()` says `ok: false`, the
  caller's documented default applies; KV appears → value within 1 s.
- [ ] `Projector` failure → the WP-1 policy write refuses with 503 and no
  row (re-run WP-1's test against the real projector).
- [ ] Cell property tests pass; the ring guarantee tested against brute
  force on 10 000 random points.
- [ ] Lint, race, coverage ≥ 85 % on `bus`, `cell`; `CHANGELOG.md`; PR
  with outputs.

## Safety notes

- The writer is the only process that opens TimescaleDB for writing; a
  second writer is a review failure.
- A dropped row is counted and logged with its subject and time range;
  never a silent drop.
- The cell is internal. A test asserts no `cell` field appears in any
  schema under `schemas/` that is marked external.

## Commits

`feat(bus): JetStream streams, KV buckets, envelope and subjects [WP-6 S-M0]`,
`feat(bus): projector and followers with age [WP-6 S-M0]`,
`feat(cell): pure-Go partition grid with ring guarantee [WP-6 S-M0]`,
`feat(tsdb-writer): batched COPY with bounded queue and dedupe [WP-6 S-M0]`.
