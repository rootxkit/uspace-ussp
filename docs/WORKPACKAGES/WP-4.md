# WP-4: CIS cache and geo-zone evaluation

Branch `feat/WP-4-cis-cache`. Milestone S-M1. Owns `internal/cis/`, the
endpoint `POST /v1/cis/notify`, the tables `cis_datasets`, `cis_features`,
`cis_notifications` (queries), KV `cis_current` (through WP-6's projector
interface; in-memory projector until WP-6 merges), the `zone/applicable`
evaluation that `intent`, `geo` and `monitor` call. Depends on WP-1.
Consumers: WP-7 (intent checks), WP-12 (geo-awareness, restriction
alerts), WP-10/11 (`monitor` zones).

## Read first

1. `docs/PLAN.md §3.2`, `§4` (`cis`), `§5.1` (cis tables), `§7` (`cis.v1`,
   KV `cis_current`), `§8` (T9), `§15` Q8.
2. Spec `02 F3` in full (pull, versions, change feed, webhook, 60 s
   reconciliation, `cis_stale` bound 300 s), `02 F1` (what a dataset
   carries, `USPACE` extended properties), `04 §3.4` (`cis/change/v1`,
   `zone/applicable/v1`), `09 §1.7` (ED-318 rows).
3. `uspace-core/ed318` doc.go in full (`Parse`, `ToZones`, `Daylight`,
   `NOAADaylight`, refusals, `PartIdentifier`), `core/zones` (`Zone`,
   `Index`, `AppliesAt`, `ContainsHorizontally`, `JudgeVertical`, `Env`,
   `Policy`), `core/ed269.Problems`, `core/geoid.Undulator`,
   `core/terrain.Ground`; vectors `zones_applicability.json`,
   `zones_vertical.json`, `ed318_roundtrip.json` (cases owned by `ussp`).
4. LESSONS Z-12 (polling gives a minute; a pushed restriction must do
   better), Z-09 (a limit that cannot be judged warns for the zones that
   matter), T-09, D-04, E-02, SC-12, SC-13, SC-22.

## What to build

- `cis.Client`: `GET /v1/{dataset}` with `If-None-Match` (ETag =
  version), `GET /v1/{dataset}/versions/{v}`, `GET /v1/changes?since=`,
  `POST /v1/subscriptions` at start (callback `USSP_USS_BASE_URL +
  /v1/cis/notify`, datasets `zones`, `uspace_airspace`, `ussp_list`,
  `restrictions`, bbox from config), ecosystem token with `cis.read`,
  body cap, deadline; the client is built from `02 F3` with `// CONTRACT:
  pending uspace-cisp openapi` and swapped for the generated client when
  the lab aggregates it.
- `cis.Receiver`: `POST /v1/cis/notify` verifying the JWS with the
  CISP's JWKS (allow-listed issuer, `aud` = us), storing the
  notification, and triggering a pull of that dataset; replay (same
  `version` twice) is idempotent and counted.
- `cis.Reconciler`: every 60 s `HEAD`/conditional `GET` per dataset; a
  missed webhook costs ≤ 60 s (prove it: kill the receiver during a
  change in the integration test).
- `cis.Store`: a dataset version is accepted whole or refused whole
  (`core/ed318.Parse` with `ed269.DefaultLimits`; refusal → counted,
  reported on `/readyz` as `cis: degraded (last publication refused:
  <first problem>)`, previous version kept). Features written with
  `geom`, limits in metres with their references, `applicable_from/to`
  from `limitedApplicability` (an open period = null), `zone_type`;
  `cis_datasets` row with `fetched_at`, `signature_ok`.
- `cis.Projection` (`zone/applicable/v1` per `cell5` into KV
  `cis_current`): per dataset version, the `zones.Zone` list (via
  `ed318.ToZones` with `NOAADaylight`) indexed by cell, plus the U-space
  airspaces with their Art. 3(4) requirements (`services_required`,
  `uas_requirements`, `service_performance`, `operational_conditions`,
  `airspace_constraints`, `adjacent`, `in_controlled_airspace`) and the
  restrictions with state. `cis_version` and `fetched_at` on every entry.
- `cis.Evaluator` (what others call; a package call, no hop):
  - `AirspacesAt(pt core.LatLon, altAMSLM float64, at time.Time)`:
    U-space airspaces containing the point (horizontal, vertical in the
    airspace's reference through `core/zones`, applicability at `at`).
  - `ZonesFor(envelope geodesy.BBox, from, to time.Time)`: candidate
    zones for an intent, each with how it applies over the window.
  - `JudgePoint(pt, aircraft zones.Aircraft, env zones.Env, at)`: the
    zones containing the point that apply at `at`, with
    `core/zones.JudgeVertical` results (`within_band`, `limit_not_judged`,
    `not_judged[]`), for `monitor` and `geo`.
  - `Age() (version string, ageS float64, stale bool)` with the 300 s
    bound from policy (`CISStaleS`).
- Counters: `cis_pulls`, `cis_pull_failed`, `cis_refused_publications`,
  `cis_webhooks`, `cis_webhook_bad_signature`, `cis_reconcile_catchups`.
- `/readyz` entry `cis` with version and age (E-02: start without a CISP
  and read what it says: `cis: unknown (no version loaded)`; with a stale
  one: `degraded (age 420 s > 300 s)`).

## Done when

- [ ] `zones_applicability.json`, `zones_vertical.json` and
  `ed318_roundtrip.json` cases owned by `ussp` pass through
  `cis.Evaluator`'s adapter (`RunOwned`): the mapping from our stored
  feature to `core/zones` inputs, never a second judgement.
- [ ] Integration (fake CISP): publish v1 → cache has it; publish v2 with
  one zone changed → webhook arrives → delta pulled within 1 s; kill the
  receiver, publish v3 → reconciliation picks it up within 60 s;
  publish a malformed collection → refused, v3 kept, `/readyz`
  degraded, counter moved (E-01 twin: a valid v4 clears the degraded
  state).
- [ ] `Age()` crosses 300 s with the fake CISP down → `stale: true`; back
  → false (both directions).
- [ ] SC-13 in unit form: a PROHIBITED 0–120 m AGL zone with
  `GroundNotConfigured` → `JudgePoint` returns warning-grade
  `limit_not_judged` with `not_judged: ["AGL"]`; a CONDITIONAL one
  returns not evaluated and counts (both asserted).
- [ ] Lint, race, coverage ≥ 85 % on `internal/cis`; `CHANGELOG.md`; PR
  with outputs.

## Safety notes

- Never repair a publication (06 T9). A zone with a limit we cannot
  judge is kept and reported, not dropped.
- `cis_version` and `cis_age_s` go on every output that rests on this
  cache (geo responses, decisions, zone alerts); WP-7/12 read them from
  `Age()`, do not recompute.
- The projection must land within one telemetry tick of the webhook
  (Z-12): measure it in the integration test and print it.

## Commits

`feat(cis): pull, versions and change feed with ETag [WP-4 S-M1]`,
`feat(cis): signed webhook receiver and 60 s reconciliation [WP-4 S-M1]`,
`feat(cis): cache store and zone/applicable projection per cell [WP-4 S-M1]`,
`feat(cis): evaluator over core zones with staleness [WP-4 S-M1]`,
`test(cis): run the owned zone vectors through the evaluator [WP-4 S-M1]`.
