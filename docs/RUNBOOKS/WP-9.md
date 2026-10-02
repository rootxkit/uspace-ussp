# WP-9 runs: the F3411 network identification Service Provider (S-M2)

The brief's S-M2 clause "the authority's DP view shows them" is to be
run by the lab against the authority's `dp-poller` (or its fake) and
recorded here with its date and image digest. **That lab run has not
been made**: the lab does not yet start this USSP's image beside the
InterUSS DSS of the compose `demo` profile (WP-20) and the authority's
Display Provider. It stays owed and is to be recorded below when the
lab runs it.

What was run is the brief's Display Provider path **in process**: rid-sp
and api against the real PostgreSQL + PostGIS, TimescaleDB and NATS
JetStream of `deploy/compose`, `internal/dss/fakedss` as the DSS, and the
integration suite's fake authority as the token service and as the
Display Provider (it subscribes in the DSS, discovers our ISA by area and
polls the ISA's `uss_base_url`). Tracks and flight facts are published on
`trk.v1` and `flight.v1` as telemetry-ingest publishes them. The tests
are `test/integration/ridsp_test.go`.

## Run 1: 2026-10-02, in process

- Code: `feat/WP-9-rid-sp` at `4222e25` (Windows 11, Go 1.27.1,
  `go test -tags integration -count=1 -p 1 -v ./test/integration/...
  ./internal/bus/... ./internal/app/tsdbwriter/...`: 114 top-level tests
  passed, none failed or skipped).
- Images (local compose project `ussp-wp9`, stopped and removed after
  the run):
  `timescale/timescaledb-ha:pg16@sha256:e9f34d8dd27621ec3933386eb07b70021d15b8116247362e0537bbff94ef0769`,
  `nats:2-alpine@sha256:ac8f88a6494bffc2c2a5289a0ca61cb28a9145c11ba5677cf24265d07f46d8d4`.
- Policy: the defaults (`rid_recent_positions_max_count` 120,
  `session_isa_radius_m` 2000, `session_isa_horizon_s` 3600).

| Test | What | Result |
|---|---|---|
| `TestIntegrationRIDSPNetworkIdentification` | flight start -> ISA in the DSS with rid-sp's base URL; the DP's subscription notified; the DP discovers the ISA, `GET /uss/flights` (1.5 km view, 60 s of recent positions) returns the flight with position, alt, height, track, speed, status, timestamp; details with operator_id, serial, operator location and utm_id; 8 km view 413; details with `rid.service_provider` only 403; flight end -> ISA deleted and the DP notified of the deletion | pass; ISA 2.0 s after the start, deleted 2.0 s after the end |
| `TestIntegrationRIDSPDSSDownAtFlightStart` | DSS down for 30 s across the flight start: `/uss/flights` serves the flight, api's `/readyz` says `dss` down since T with one ISA write waiting; DSS back -> ISA created, `dss` up, the outbox empty | pass; ISA 8.0 s after the DSS came back |
| `TestIntegrationRIDSPISANotification` | a peer's ISA notification stored in `rid_isa_notifications` (204); another sender 403; a display provider token 403 | pass |
| `TestIntegrationRIDSPLoad` | 100 flights at 1 Hz, 10 views (1.5 to 6.45 km) polled at 1 Hz with 60 s of recent positions, for 60 s | 570 requests, 26 173 flights served, none failed, none older than 60 s; alone p50 13 ms, p95 38 ms, p99 46 ms, max 68 ms; inside the full suite p50 20 ms, p95 187 ms, p99 339 ms, max 434 ms (budget: p95 1 s, p99 3 s) |

## Lab run (owed)

To be recorded: date, the image digest of `ghcr.io/rootxkit/uspace-ussp`,
the InterUSS DSS image, the authority's `dp-poller` image, and what its
DP view showed for a SITL aircraft streamed through `WS /v1/telemetry`.
