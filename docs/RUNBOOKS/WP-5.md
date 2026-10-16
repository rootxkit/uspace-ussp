# WP-5 runs: registry answers under the change feed (SC-17, USSP clause)

The fix for the staging fault (an answer fetched just after a
registration deleted by the change feed; the aircraft `unknown_operator`
/ `registry_unavailable` for its whole flight and a spurious critical
`identification` alert inside a PROHIBITED zone) is an alert path
(CLAUDE.md rule 6, INV-02). Its scenario is SC-17's USSP clause
(uspace-lab `knowledge/scenarios.md`).

**Where it runs.** `web/e2e/registry.spec.ts` against the browser-test
stack `test/e2e/stack` (the seven processes on real PostgreSQL + PostGIS,
TimescaleDB and NATS JetStream, the fake authority whose feed api polls
every 30 s, the fake CISP with the PROHIBITED zone TZE2E17 in AMSL). CI's
web job runs it on every pull request that touches Go or the web
(`pnpm test:e2e`), so CI holds the scenario.

**What is not run.** The scenario has not been run in the uspace-lab
stack (the lab images with the real uspace-authority, as staging ran
it): the lab's scenario runner does not drive this USSP's intents API
and no lab run was made for this change. It is owed, and is to be
recorded here with its image digests and numbers when it is run.

## The scenario

1. An operator and its UAS are registered at the authority (changes 1
   and 2 on its feed); the operator account and the intent are decided
   at once, so the decision's lookup is fetched under a feed cursor
   behind the registration (the staging order).
2. The aircraft flies inside TZE2E17 with its intent 2 to 6 km east of
   the zone.
3. The feed applies the page naming the registration (`/readyz`
   `feed_cursor` at or beyond it). Expected: the zone incursion is
   raised (the zone path judged the aircraft); for the next two poll
   intervals the own track is `registered` in the intent's traffic
   product and no `identification` alert exists for the intent.
4. The authority suspends the UAS. Expected: once the feed has applied
   it, the track is `suspended` within 15 s; still no `identification`
   alert (a suspended aircraft is not an incident status).
5. Presence twin: an aircraft whose serial the registry does not hold,
   flown without an intent 200 m east in the same zone. Expected: the
   `identification` alert is raised for its flight.

## Run 1: 2026-10-16, local, the stack on Windows

- Host: Windows 11, Go 1.27.1, Node 22, Playwright from `web/` (only the
  API-level spec; the portal build was not part of this run, so the
  spec ran under a local configuration without the web server).
- Images: `timescale/timescaledb-ha:pg16@sha256:e9f34d8dd27621ec3933386eb07b70021d15b8116247362e0537bbff94ef0769`
  (local compose deps, shared with `make integration`, the registry
  tables emptied before each run), `nats:2-alpine@sha256:ac8f88a6494bffc2c2a5289a0ca61cb28a9145c11ba5677cf24265d07f46d8d4`
  (plain JetStream, as the CI web job).

| Run | Code | Result |
|---|---|---|
| Before the fix | `2a30b3e` (main) with this branch's `test/e2e/stack/main.go` only | **fails** at step 3: the own track `unknown_operator` once the feed applied the registration; `identification` critical raised 15 s after the flight started (09:44:04Z) |
| With the fix | the tree of `108c937` (this branch) | **passes** in 2.0 min: steps 3 to 5 as expected |

Alerts recorded by the run with the fix (from 09:44:30Z): `zone_incursion`
critical for the registered flight (09:44:36Z), and `zone_incursion` plus
`identification` critical only for the unregistered twin (09:46:32Z).

api's counters at the end of the run with the fix: `registry_feed_polled`
4, `registry_feed_invalidated` 3, `registry_feed_refetched` 3,
`registry_feed_retained` 0, `registry_refetch_failed` 0,
`registry_warmed` 2, `registry_revalidated` 849 and `registry_fetched`
856 (the sweep also asked about the bound keys of every open flight and
active intent the shared database held from earlier integration runs).

## Run 2: 2026-10-16, staging droplet, the fix on the deployed stack

This is the staging re-run of the fault the fix is for, flown on the
deployed stack. It is not the lab-stack SC-17 run: that run, against the
real uspace-authority in the uspace-lab stack, is still owed (see "What
is not run" above) and is to be recorded here when it is made.

- Flight (observed): demo flight by `tools/demo-flight.py` of
  uspace-deploy, UAS `TESTA000DEMFLT1`, operator `GEODEMOFLT0001`,
  intent `8f8980d8-aae8-4c12-a092-932b7d88393a`, 1 Hz until 13:12:01Z.
  The operator and the UAS had been registered at 08:09Z the same day.
- Restriction (observed): ANSP restriction `DARH21J`, PROHIBITED, a 500 m
  circle at 41.745, 44.745, 0 to 120 m AGL, window 12:56 to 13:05Z.
  Planned at 12:52:41Z with activation scheduled before the start.
- Read-only evidence collected from the droplet at about 13:00Z
  (`docker inspect`, `docker logs`, SELECT on `ussp_relational`).

**Images.** `docker inspect` of every `uspace-ussp-*` process container
(api, monitor, telemetry-ingest, tsdb-writer, dss-sync, traffic-ws,
rid-sp) reports the same image id `sha256:72f4c4e28b14...` and the
repo digest below, which is the pin in `compose/images.env`
(`USSP_GO_IMAGE`, uspace-ussp `eed00cd`). The relational migration
version is 31 (`goose_db_version_relational`, highest applied).

| Image | Digest | Matches `compose/images.env` |
|---|---|---|
| ussp Go (`ghcr.io/rootxkit/uspace-ussp`) | `sha256:544ba563e9279a3d5a05bb7cd59ee3e0a8ef15ce4e32294ec652a0afb07a1e3e` | yes |

**What happened.**

| Time (UTC) | Event | Observed |
|---|---|---|
| 12:52:41 | `DARH21J` planned | activation scheduled before the start |
| 12:56:00 | `DARH21J` activated by the system (v2) | CISP v2 published, DSS written, USSPs notified at 12:56:00, no delivery alarm |
| 12:56:01 | USSP alerts raised (DB `raised_at`) | `restriction_activated` `58e95f44-2877-4d6d-aaaf-eeb2266ccdf6` (12:56:01.16) and `zone_incursion` `bf04ec00-35aa-4c8a-ab87-c71a9b480cc8` (12:56:01.94), both critical |
| 12:56:28 | `DARH21J` ended by hand (v3) | CISP v3 published, DSS deleted, USSPs notified at 12:56:28, no alarm |
| 12:56:28 | `zone_incursion` cleared (DB) | `not_reconfirmed` at 12:56:28.94 |
| 12:56:29 | `restriction_activated` cleared (DB) | `resolved` at 12:56:29.12 |

No `identification` alert exists for the intent or for its flight
(`07462b5b-812c-4206-883e-c1717d11ac22`): a SELECT on `alerts` for the
intent returns exactly the two rows above. The `zone_incursion` row
carries `height_agl_m` 59.1 and `restriction_state` `active`; the
`restriction_activated` row lists the intent in `affected_intents`.

**Contrast with the morning, before the fix.** At 08:14Z the same kind
of flight (intent `50fbd7bc-69c5-435c-8992-f93eca3efaa2`, restriction
`DARVMFO`) raised `identification` `4a0fa223-d2c2-456f-8e23-942e149753f3`
with `status` `unknown_operator` and `identification_reason`
`registry_unavailable`, cleared `not_reconfirmed` at 08:14:51Z.

**ussp-api registry counters** (status lines of `uspace-ussp-api-1`,
one per minute, cumulative since its start at 11:37:20Z, no restart).
Every other registry counter in the lines is 0 or equal throughout:
`registry_cache_hit` 6, `registry_cache_miss` 2, `registry_warmed` 2,
`registry_revalidate_failed` 0, `registry_write_skipped` 0.

| Status line (UTC) | `registry_feed_not_modified` | `registry_fetched` | `registry_revalidated` |
|---|---|---|---|
| 12:50:21 | 147 | 40 | 38 |
| 12:52:21 | 151 | 40 | 38 |
| 12:55:21 | 157 | 46 | 44 |
| 12:56:21 | 159 | 48 | 46 |
| 12:57:21 | 161 | 50 | 48 |
| 13:00:21 | 167 | 56 | 54 |

`registry_cache_hit` went from 2 to 6 and `registry_warmed` from 1 to 2
at the 12:52:21 line, when the flight's intent was submitted.

**Not in the logs.** `registry_feed_polled`, `registry_feed_invalidated`,
`registry_feed_refetched`, `registry_feed_retained`,
`registry_refetch_failed` and `ident_registry_unavailable` appear in no
status line and no other line of any `uspace-ussp-*` container for
12:50 to 13:00Z; whether they were never incremented or are not emitted
in these lines is not established here. The status lines were collected
up to 13:00:21Z, so the rest of the 12:50 to 13:15Z window (including
the end of the flight at 13:12:01Z) is not covered by the counters
above. The api container logged 25 WARN or ERROR lines from 12:50Z;
they were not examined for this record.
