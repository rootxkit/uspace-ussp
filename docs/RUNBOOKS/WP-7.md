# WP-7 runs: flight authorisation (S-M1)

The brief asks for a recorded lab scenario before merge (CLAUDE.md
"safety-relevant PRs"). The lab's scenario runner does not drive this
USSP yet: uspace-lab has no runner for the USSP's intents API, and WP-7
has no alert path (it decides; conformance, WP-10, is the first alert path
that needs SITL). The S-M1 scenario is therefore run **in process**,
against the real PostgreSQL + PostGIS, TimescaleDB and NATS JetStream
of `deploy/compose` and the fakes of `internal/testfakes` (the CISP with
signed publications, the authority's F8), through the HTTP API with
operator tokens of this USSP's own issuer. It is
`test/integration/intents_test.go` `TestIntegrationScenarioSM1`; the
lab scenario stays owed and is to be recorded here when the lab runs it
against this repo's image.

## Run 1: 2026-10-02, in process

- Commit: `0ebf324` on `feat/WP-7-intent-authorisation` (Windows 11, Go
  1.27.1, `go test -tags integration -count=1 -p 1`).
- Images (local compose project `ussp-wp7`, stopped after the run):
  `timescale/timescaledb-ha:pg16@sha256:e9f34d8dd27621ec3933386eb07b70021d15b8116247362e0537bbff94ef0769`,
  `nats:2-alpine@sha256:ac8f88a6494bffc2c2a5289a0ca61cb28a9145c11ba5677cf24265d07f46d8d4`.
- Geoid: a constant grid (N = 20 m) loaded by uspace-core `geoid.Load`;
  no terrain (AGL limits not judged, as in the api process today).
- Policy: the defaults (`policy_version` 1 of the rig): deviation
  50 m / 15 m / 60 s, special operations at priority 100, no buffers,
  activation from 600 s before `time_start`.

| Step | What | Decision | Reasons |
|---:|---|---|---|
| 1 | Operator A files inside U-space airspace SC-USP | authorised (47 ms, number `USSP-DEV-GEO-TEST-…-<ULID>`) | none |
| 2 | Operator B files the same volume later | rejected | `intent_filed_first` (names A's intent) |
| 3 | A files into PROHIBITED zone SC-ZONE | rejected | `zone_prohibited` |
| 4 | B files a special operation over A | authorised | `intent_flagged_for_update` (A's row carries `update_required`) |
| 5 | The ANSP publishes restriction SC-DAR; A files over it | rejected | `restriction_active` |
| 6 | A newer `uspace_airspace` version arrives unsigned (held); A files | rejected | `cis_outdated` |
| 7 | A trusted version replaces it; A files again | authorised | none |
| 8 | The DSS is down; A files inside U-space airspace | pending_dss | `dss_unavailable` |
| 9 | The DSS is down; A files outside U-space airspace | authorised | none (local checks suffice, 02 F5) |
| 10 | A files a C0 A1 flight | accepted_voluntary | none, no number |
| 11 | The authority suspends B; the F8 feed invalidates the cache; B files | rejected | `operator_suspended` (item 10) |
| 12 | A activates an hour before its window | 409 `activation_refused` | |
| 13 | A ends its intent | ended | out of `intent_active` |

Counters at the end of the run: `intent_submitted` 11,
`intent_decision_authorised` 4, `intent_decision_rejected` 5,
`intent_decision_pending_dss` 1, `intent_decision_accepted_voluntary` 1,
`intent_activation_refused` 1, `intent_ended` 1, `published_intent` 12.

The same run's suite: 48 integration tests passed (`test/integration`),
among them every S-M1 done-when clause as its own test and
`TestIntegrationIntentLatencyWith1000Active`: with 1000 authorised
intents sharing the outline and the window (so the store's prefilter
returns all 1000 and the deconfliction judges each), 40 further decisions
took p50 82 ms, p95 105 ms, max 126 ms (budget p95 500 ms, PLAN §9).

## Run 2: 2026-10-02, in process, after the review fixes

- Commit: `d93b14d` on `feat/WP-7-intent-authorisation` (Windows 11, Go
  1.27.1, `go test -tags integration -count=1 -p 1 -v ./test/integration/...
  ./internal/bus/... ./internal/app/tsdbwriter/...`), relational schema
  at migration 00012.
- Images (local compose project `uspace-ussp`):
  `timescale/timescaledb-ha:pg16@sha256:e9f34d8dd27621ec3933386eb07b70021d15b8116247362e0537bbff94ef0769`,
  `nats:2-alpine@sha256:ac8f88a6494bffc2c2a5289a0ca61cb28a9145c11ba5677cf24265d07f46d8d4`.
- Geoid, terrain and policy as in run 1. Aircraft A is held by the fake
  authority as class C0 (the exemption now rests on the F8 answer); the
  registry change feed's cursor is reset at the start of the scenario.

| Step | What | Decision | Reasons |
|---:|---|---|---|
| 1 | Operator A files inside U-space airspace SC-USP | authorised (41 ms) | none |
| 2 | Operator B files the same volume later | rejected | `intent_filed_first` |
| 3 | A files into PROHIBITED zone SC-ZONE | rejected | `zone_prohibited` |
| 4 | B files a special operation over A | **rejected** (changed from run 1) | `intent_filed_first`; condition `special_operation_unverified`; A is not flagged (PLAN §15.2 Q19) |
| 5 | The ANSP publishes restriction SC-DAR; A files over it | rejected | `restriction_active` |
| 6 | A newer `uspace_airspace` version arrives unsigned (held); A files | rejected | `cis_outdated` |
| 7 | A trusted version replaces it; A files again | authorised | none |
| 8 | The DSS is down; A files inside U-space airspace | pending_dss | `dss_unavailable` |
| 9 | The DSS is down; A files outside U-space airspace | authorised | none |
| 10 | A files a C0 A1 flight (F8 holds C0) | accepted_voluntary | none, no number |
| 11 | The authority suspends B; the F8 feed invalidates the cache; B files | rejected | `operator_suspended` |
| 12 | A activates an hour before its window | 409 `activation_refused` | |
| 13 | A ends its intent | ended | out of `intent_active` |

Counters at the end of the run: `intent_submitted` 11,
`intent_decision_authorised` 3, `intent_decision_rejected` 6,
`intent_decision_pending_dss` 1, `intent_decision_accepted_voluntary` 1,
`intent_special_operation_unverified` 1, `intent_intent_filed_first` 2,
`intent_activation_refused` 1, `intent_ended` 1, `published_intent` 12.

The same run's suite: 61 top-level tests passed in `test/integration`
(36 more in `internal/bus` and `internal/app/tsdbwriter`), none skipped,
among them the review's new ones:
`TestIntegrationIntentFirstComeRanksAfterTheLock` (two decisions raced
on PostgreSQL, the first holding its transaction between BEGIN and the
lock: exactly one authorised, nothing flagged),
`TestIntegrationIntentActivationRefusedWhileUpdateRequired` and
`TestIntegrationIntentProjectedAfterCommit` (a failed commit leaves no
row and no `intent_active` key; a bus failure after the commit is
republished). `TestIntegrationIntentLatencyWith1000Active`: p50 78 ms,
p95 91 ms, max 105 ms (budget p95 500 ms).
