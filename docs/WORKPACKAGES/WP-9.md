# WP-9: F3411 network Remote ID Service Provider

Branch `feat/WP-9-rid-sp`. Milestone S-M2. Owns `internal/ridsp/`,
`internal/app/rid-sp` and `cmd/rid-sp`, the F3411 USS endpoints
(`GET /uss/flights`, `GET /uss/flights/{id}/details`,
`POST /uss/identification_service_areas/{id}`), ISA upkeep in the DSS
(`dss_isas` queries), the optional `WS /v1/authority/flights`. Depends on
WP-3 (generated server and DSS client), WP-8 (`trk.v1`, flights).
Consumers: the authority's Display Provider, peer USSPs, WP-14 (ISA
notifications for our peer views).

## Read first

1. `docs/PLAN.md §2` D12, `§3.1` (`rid-sp`), `§6.2`, `§9` (SP budget),
   `§15` Q14.
2. Spec `01 §3` S1 and S10 (Art. 8(2) content, 1 Hz, response times),
   `02 F6` F3411 paragraph (ISA per flight, subscriptions ≤ 10 / 24 h,
   `GET /uss/flights?view=` ≤ 7 km, details ≤ 2 km, p95 1 s / p99 3 s,
   positions ≤ 60 s old plus `recent_positions`), `02 F7` (what the
   authority does as a DP; the optional push), `04 §3.1` (`RIDFlight` ...
   mapping to Art. 8(2)(a)–(g), special values), `05 §5` (DP polls: one
   in-flight per view), `06 §5` (`operator_location` only to authorised
   DPs; public subset excludes it), `09 §1.4` every F3411 row.
3. `uspace-core/f3411` doc.go and the generated types (`RIDFlight`,
   `RIDAircraftState`, `RIDAircraftPosition`, `RIDHeight`, `RIDFlightDetails`,
   `UASID`, `OperatorLocation`, `UAClassificationEU`, `IdentificationServiceArea`,
   `Volume4D`), the `Net*` constants (use them, never a literal),
   `SpecialSpeed` and friends (null → special value on the way out),
   `Scope` constants; WP-3's `stdapi/f3411` server and DSS client.
4. LESSONS R-05 (every display says what is broadcast vs authenticated —
   here: `trust: authenticated` is what we serve, as the SP), R-14,
   E-02, SC-16 (a provider switched off: what the DP side sees).

## What to build

- `ridsp.Window`: per flight, the last `NetMaxNearRealTimeDataPeriodSeconds`
  (60 s) of `track/telemetry/v1` from `trk.v1.>` in memory (ring per
  flight, bounded by `policy.RIDRecentPositionsMax`), indexed by `cell5`
  for view queries; no database on the request path.
- `GET /uss/flights?view=&recent_positions_duration=`: scope
  `rid.display_provider`; `view` parsed as the standard's
  `lat,lng,lat,lng`; diagonal > `NetMaxDisplayAreaDiagonalKm` → 413 with
  the standard's error body; flights whose current position or recent
  positions intersect the view; `RIDAircraftState` from our track (
  `timestamp`, `timestamp_accuracy`, `operational_status` from our
  status, `position {lat, lng, alt (HAE), accuracy_h/v, extrapolated:
  false, pressure_altitude, height {distance, reference}}`, `track`,
  `speed`, `speed_accuracy`, `vertical_speed`; nulls become the standard's
  special values on the wire); `recent_positions` limited to the
  requested duration ≤ 60 s; `simulated: false`; `aircraft_type` from
  the intent's declaration; response `timestamp` = now. Deadline so the
  p99 budget holds: the handler never blocks on anything but memory.
- `GET /uss/flights/{id}/details`: `uas_id {serial_number,
  registration_id (UA registration when applicable), utm_id (flight
  id), specific_session_id: null}`, `operator_id` (registration number,
  public part), `operator_location` (remote pilot or take-off position
  from the last frame, with `altitude_type`), `operation_description`
  (the authorisation number when present), `eu_classification`
  (category, class from the intent's Annex IV block). Served only to
  `rid.display_provider`; the public subset is not served by us (Q14).
- `ridsp.ISA`: on flight start, `PUT /rid/v2/dss/identification_service_areas/{id}`
  with extents = the intent's volumes (or, without an intent, a circle
  of `policy.SessionISARadiusM` around the first position, time window
  now + `policy.SessionISAHorizonS`), `uss_base_url = USSP_USS_BASE_URL`;
  on each DSS response, notify the `subscribers` the DSS lists
  (`POST {url}/uss/identification_service_areas/{id}` with the
  `subscription_state` list, scope `rid.service_provider`, `aud` = the
  host of each subscriber's `uss_base_url`; reconciliation M6, M18: the
  authority verifies exactly this) within the standard's time; on flight end,
  `DELETE` with the version; retries with backoff through the
  `dss_outbox`; DSS down → flights are still served on `/uss/flights`,
  `/readyz` says `dss: down since T`, ISA writes replay on recovery.
  ISA versions kept in `dss_isas`.
- `POST /uss/identification_service_areas/{id}` (we are also a DP for
  peers, WP-14): scope `rid.service_provider`, sent by the peer SP (not
  the DSS) with `aud` = our host; stores the notification for WP-14's
  view registry; 204.
- Optional `WS /v1/authority/flights` (scope `rid.display_provider`,
  only when `USSP_AUTHORITY_PUSH=on`): 1 Hz `RIDFlight` frames for every
  airborne flight; bounded 10 min buffer, then drop oldest with a gap
  record (02 F7). Off by default; a test proves it is absent (404) when
  off and present when on.
- Metrics: `rid_sp_flights_requests`, latency histogram, `rid_sp_view_too_large`,
  `rid_sp_isa_writes`, `rid_sp_isa_failed`, `rid_sp_subscriber_notify_failed`.

## Done when

- [ ] Against the lab's InterUSS DSS (compose `demo` profile, WP-20
  provides it; in CI the WP-6-style fake DSS from `internal/dss/fakedss`
  with ISA semantics): flight start → ISA exists in the DSS with our
  base URL; the authority-role fake DP discovers it and `GET /uss/flights`
  returns the flight with every Art. 8(2) item mapped as `04 §3.1` says;
  details served for a 1.5 km view, 413 for an 8 km view; flight end →
  ISA deleted (E-01 both ways on every cap).
- [ ] Load: 100 flights at 1 Hz, 10 views polled at 1 Hz → p95 ≤ 1 s,
  p99 ≤ 3 s measured in the integration test and printed; nothing older
  than 60 s in any response.
- [ ] E-02: DSS down for 30 s during a flight start → `/uss/flights`
  still serves, `/readyz` says why, ISA created on recovery; DSS up →
  `dss: up` and the outbox empty.
- [ ] The S-M2 clause "the authority's DP view shows them" run by the lab
  against the authority's `dp-poller` (or its fake) and recorded in
  `docs/RUNBOOKS/WP-9.md` with date and image digest.
- [ ] Special values: a track with unknown speed serves `255`, unknown
  track `361`, unknown height `-1000` (tests through core's constants,
  not literals); the reverse (`f3411.UnmarshalRIDFlight` of our own
  output) yields nils.
- [ ] Lint, race, coverage ≥ 85 % on `ridsp`; `CHANGELOG.md`; PR with
  the measured latencies.

## Safety notes

- We serve what the operator sent, marked `authenticated`; we never fuse
  a broadcast into it here (that is the authority's and WP-14's
  concern with the spoofing guard).
- `operator_location` is personal data of the remote pilot: served to
  `rid.display_provider` only, never logged at info level, never in the
  authority push beyond the standard's shape.
- The push extension can never be a reason to skip or delay the ISA and
  `/uss/flights` path; a test asserts the SP path works with the push
  disabled.

## Commits

`feat(ridsp): 60 s flight window and GET /uss/flights [WP-9 S-M2]`,
`feat(ridsp): flight details for authorised display providers [WP-9 S-M2]`,
`feat(ridsp): ISA lifecycle in the DSS with subscriber notification [WP-9 S-M2]`,
`feat(ridsp): optional authority flight push, off by default [WP-9 S-M2]`.
