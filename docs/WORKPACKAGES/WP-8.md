# WP-8: telemetry ingest and flights

Branch `feat/WP-8-telemetry-ingest`. Milestone S-M2. Owns
`internal/telemetry/`, `internal/flights/`, `internal/app/telemetry-ingest`
and `cmd/telemetry-ingest`, the endpoints `WS /v1/telemetry` and
`POST /v1/telemetry/batch`, the subjects `trk.v1.*`, `ingest.v1.*`,
`src.v1.*`, `ident.v1.*` (producer side), the schemas `telemetry/v1` and
`track/telemetry/v1`. Depends on WP-2 (scopes, bindings), WP-5
(identification lookup), WP-6 (bus, cell). Consumers: WP-9, WP-10, WP-11,
WP-15.

## Read first

1. `docs/PLAN.md §3.1` (`telemetry-ingest`), `§4`, `§5.2` (`telemetry`),
   `§7`, `§8` (T3, T11), `§9`.
2. Spec `02 F5` network ID telemetry row (fields, 1 Hz, `backlog`, the
   10 min client queue, `telemetry_lost` 5 s), `04 §2` (envelope, trust,
   `source`), `04 §3.1` (`track/telemetry/v1`, altitude rules), `04 §3.2`
   (identification carried inside every track), `05 §5` (backpressure:
   2 Hz per client, over-rate dropped and counted, the `ingest.v1` work
   queue, oldest dropped never newest), `06 §2` T3 (serial binding,
   two sessions, teleport), T11.
3. `uspace-core/timeplace` (`PlaceBatch`, `Times`), `core/geoid`
   (`AMSLFromHAE`), `core/rid.SelectAltitude` and `AltitudeSelector` (the
   pressure fallback and hold apply to operator telemetry too when it
   carries `alt_pressure_m` and a poor geodetic accuracy), `core/identify`
   (`ResolveBound`), `core/serial.FoldKey`, `core/sources.Follower`;
   vectors `rid_time.json` (network cases), `pressure_altitude.json`
   (`ussp` cases), `fleet_match.json` (via WP-5's adapter).
4. LESSONS T-01, T-02, T-03, T-04 (history never alerted), T-11 (a stalled
   reader must not stamp old input as live), T-13, R-07 (no geoid, no
   AMSL), R-16, B-01, B-03, B-04, B-05, B-10, B-11, B-12, B-14, SC-09,
   SC-14, SC-15, SC-22.

## What to build

- `schemas/telemetry/v1.json` (what an operator client sends: `ts`,
  `serial`, `position`, `alt_wgs84_m`, `alt_pressure_m?`, `height_m` +
  `height_ref`, `speed_ms`, `track_deg`, `vspeed_ms`, `status`,
  `emergency`, `operator_position?`, `accuracy_h`, `accuracy_v`,
  `timestamp_accuracy_s`, `intent_id?`, `seq`, `backlog`) and
  `schemas/track/telemetry/v1.json` (internal, `04 §3.1`) with examples;
  a `trust: simulated` or `source: sitl` value fails schema validation
  (T11) and the test proves both the refusal and the acceptance of a
  normal frame.
- `telemetry.Session` (one per WS connection, B-14: a reconnect replaces
  the old session and the old teardown cannot disturb the new one):
  bearer with `ussp.telemetry`, client from the token, bound serials from
  KV `client_bindings` (a frame for an unbound serial → `refused_unbound`
  counted and a `status` frame; the session stays up), source control
  (`operator_ws` type, client id instance; disabled → close 1013, HTTP
  503 with `Retry-After` on reconnect, B-10), per-client rate 2 Hz
  (over-rate frames dropped, counted, reported in the `status` frame),
  dedupe on `(serial, seq)` within 30 s, body cap per frame. Frames in
  both directions carry the common envelope (`schema` + `body`;
  reconciliation M29): `telemetry/v1` bodies from the client, and the
  server's `status` frames as `console/status/v1` bodies (`accepted`,
  `dropped`, `rate`, `acked_seq`, `backlog` as its extras) so one client
  code path parses this socket and `/v1/traffic`.
- Time placement: per frame `rx_ts` = now; per batch
  `timeplace.PlaceBatch(rxTS, ts[], 120 s)` → `captured_at` (T-02);
  `backlog` from the client's flag or from `rx_ts - captured_at >
  policy.BacklogAfterS` (default 10 s) (T-04, T-11); `time_source` as
  core says; a sample older than the held one for the serial on its own
  clock is `rejected_out_of_order` (T-03); a frame ahead of `rx_ts` by
  more than tolerance is clamped and counted (T-13).
- Altitude: `alt_amsl_m = AMSLFromHAE(alt_wgs84_m, N)` through the geoid;
  `rid.AltitudeSelector` for the pressure fallback and hold when
  `accuracy_v` is poor and `alt_pressure_m` is present; no geoid → no
  AMSL, `alt_source: none`, counted, and `/readyz` says `geoid: missing`
  (R-07, SC-22).
- Identification: `identify.ResolveBound` through WP-5's lookup for the
  session's serial (basis `authenticated`, reason `session_binding` or
  the inactive reason); emitted on `ident.v1` when it changes.
- `flights.Binder`: a frame with `intent_id` binds to that intent's
  flight (creating the `flights` row through an internal `api` call on
  the first frame; the intent must be `activated` and owned by the
  client, else `refused_intent_state` and the frame is still accepted as
  a session without intent outside U-space airspace, or refused inside
  it with the reason); a frame without `intent_id` → a flight per
  session outside U-space airspace, refused inside it
  (`refused_no_authorisation`, counted, logged with the airspace id).
  Flight end: `policy.TelemetryLostS` (5 s) of silence marks
  `telemetry_lost` on the track (not an end); `policy.FlightEndAfterS`
  (default 120 s) of silence or an operator `end` ends it; `intent_active`
  gets the `flight_id`.
- Publish `track/telemetry/v1` on `trk.v1.<cell3>.<cell5>.<track_id>`
  (`track_id = flight_id`), `trust: authenticated`, `source: operator_ws`
  or `operator_batch`, `source_instance: client_id`, with the
  identification block; `src.v1.operator_ws.<client_id>` every 2 s
  (`last_seen`, `accepted`, `refused`, `dropped_rate`, `lag_s`, `lagging`
  per B-03).
- Backpressure: when the publisher's bounded queue (`policy.IngestQueueS`,
  10 s) is full, frames go to `ingest.v1.<cell3>` (JetStream work queue,
  10 min); a drain worker in the same process replays them as `backlog`
  with their original `captured_at`; beyond 10 min the oldest are
  dropped with a `gap` record (`gap_started`, `gap_ended`, `dropped`)
  published on `src.v1` and logged, never the newest (05 §5). Acks to the
  client (`status` frame `acked_seq`) only for frames durably queued or
  published (B-05).
- `POST /v1/telemetry/batch`: same pipeline, ≤ 1 s of frames, `backlog`
  honoured, 202 with `accepted`/`refused` counts.

## Done when

- [ ] `rid_time.json` network cases and `pressure_altitude.json` `ussp`
  cases pass through the ingest adapter (`RunOwned`).
- [ ] Integration with a simulated operator client (`internal/testfakes/operator`,
  also used by the lab): 100 frames/s for 60 s → 6000 rows, `trk`
  messages with `captured_at` within the batch rule, no drop; a 60 s
  outage (close the socket) with the client queueing then draining →
  rows land as `backlog`, no live alert consumer sees them (assert the
  `backlog` flag), drain rate ≥ 5× intake measured and printed (SC-14).
- [ ] E-01 pairs: unbound serial refused / bound accepted; over-rate
  dropped / in-rate accepted; disabled source 1013 + 503 / enabled
  accepted; second session for a serial replaces the first and the first
  frame of the old session after replacement is refused; `trust:
  simulated` refused / `authenticated` accepted; frame without
  `intent_id` inside U-space airspace refused / outside accepted.
- [ ] E-02: no geoid configured → track has `alt_amsl_m: null`,
  `alt_source: none`, `/readyz` degraded, the counter moved; geoid
  configured → AMSL present and equal to HAE − N for the fixture point.
- [ ] SC-15 in unit form: 30 s of frames delivered at once with their
  own `ts` are placed by `ts` (within the batch rule) and flagged
  `backlog`, not stamped at read time.
- [ ] Lint, race, coverage ≥ 85 % on `telemetry`, `flights`;
  `CHANGELOG.md`; PR with the measured numbers.

## Safety notes

- The socket is receive-only towards the operator except for `status`
  frames (acks, counters, refusals). No frame type that could reach a GCS
  as a command exists; the schema has no such message and the test
  asserts the server never sends anything but `status`.
- `backlog` is recorded, never alerted. The flag is set here and read by
  `monitor`; if this WP sets it wrong, every alert is wrong.
- An aircraft is never hidden: a refused frame is counted and the
  session's status says why; a disabled source ages out as
  `source_disabled` downstream (B-11).

## Commits

`feat(telemetry): operator WS and batch ingest with bindings and rate limits [WP-8 S-M2]`,
`feat(telemetry): time placement, AMSL and identification on every track [WP-8 S-M2]`,
`feat(flights): bind sessions to activated intents and end flights [WP-8 S-M2]`,
`feat(telemetry): work-queue backpressure with counted gaps [WP-8 S-M2]`,
`test(telemetry): drain against intake with a simulated operator [WP-8 S-M2]`.
