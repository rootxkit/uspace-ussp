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
