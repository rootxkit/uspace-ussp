# WP-4: CIS cache and geo-zone evaluation

Branch `feat/WP-4-cis-cache`. Milestone S-M1. Owns `internal/cis/`, the
endpoint `POST /v1/cis/notifications`, the tables `cis_datasets`, `cis_features`,
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
  /v1/cis/notifications`, datasets `zones`, `uspace_airspace`,
  `ussp_list`, `restrictions`, bbox from config), ecosystem token with
  `cis.read` and `aud` = the CISP's host (M18), body cap, deadline. The
  client is generated from the pinned copy `api/clients/cisp.yaml`
  (`SOURCE` = the CISP commit; CI diffs it; M11), bumped in a `build:`
  commit, never hand-built. ED-318 metadata is core's `ed318.Metadata`
  (`issued`, `provider`) plus the CISP's top-level `cis_dataset`,
  `cis_version`, `cis_updated_at` (M15).
- `cis.Receiver`: `POST /v1/cis/notifications` (one path on every
  subscriber, M1): a compact JWS (`Content-Type: application/jose`)
  verified with the shared verifier against the allow-listed issuers
  `USSP_CIS_NOTIFY_ISSUERS`, the CISP and, on its degraded direct
  path, the ANSP, each with its JWKS URL from config (M5); `aud` = our
  host (M19), `sub` = subscription id, `jti` = delivery id (replayed
  `jti` → 204, counted). The payload is `cis/change/v1` from the CISP's
  pinned schema. Reasons `subscription_test`, `republished` and any
  reason unknown to us are acknowledged `204` with no pull (M16,
  additive-enum rule of `04 §4`); every other reason stores the
  notification and triggers a pull of that dataset. `pull_url` is
  honoured only when its host equals the issuer's configured base host
  (SSRF guard); otherwise the dataset is pulled from the configured
  CISP base URL and the mismatch is counted. Replay (same `version`
  twice) is idempotent and counted.
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
- [ ] Receiver pairs (E-01): a notification signed by the fake CISP →
  pull; the same signed by the fake ANSP's key → pull (degraded direct
  path, M5); signed by a key of neither → 401 and
  `cis_webhook_bad_signature`; reason `subscription_test` → 204 and no
  request at the fake CISP (assert its counter); reason `zones_changed`
  → one pull; `pull_url` on another host → no request to that host,
  counted, dataset pulled from the configured base URL.
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
`feat(cis): signed change-notification receiver and 60 s reconciliation [WP-4 S-M1]`,
`feat(cis): cache store and zone/applicable projection per cell [WP-4 S-M1]`,
`feat(cis): evaluator over core zones with staleness [WP-4 S-M1]`,
`test(cis): run the owned zone vectors through the evaluator [WP-4 S-M1]`.

## As built (the PR)

What the build decided where the brief, the contracts or core left a
choice; each is in the PR body as well.

- Applicability is `ed318.Applies` over the feature's
  `limitedApplicability`, at the centre of each part's bounding box;
  `ed318.ToZones` builds the shapes and limits from a copy without the
  periods. ToZones refuses a daylight schedule without end dates, which
  ED-318 allows and the CISP publishes with a warning ("judge it with
  Applies"); building it from ToZones' windows would refuse such a
  publication. An applicability that cannot be evaluated is `unknown`,
  returned and counted, never "does not apply".
- The done-when's `zones_changed` is not a `cis/change/v1` reason: by M16
  it is acknowledged 204 without a pull (tested). The pulling reasons
  are `publication` and the `restriction_*` ones (tested with
  `publication` and `restriction_activated`).
- An ANSP-signed notification is verified, logged and pulls the dataset
  from the configured CISP. Its `pull_url` (the ANSP's
  `GET /v1/restrictions/{id}`) is not followed: the ANSP's OpenAPI is not
  pinned here yet (ANSP WP-8). Spec gap; while the CISP is down the
  notified version stays on `/readyz` as "notified ... not pulled yet".
- Provenance: a new version is used only when its publisher's signature
  verifies. Once its bytes parse and build, `GET
  /v1/{dataset}/versions/{v}` is read and its `X-Publisher-Signature`
  (the detached JWS the authority or the ANSP sent with the
  publication, forwarded by the CISP as received, with
  `X-Publisher-Kid`) is verified over those bytes with core's
  `DetachedVerifier` as the dataset's publisher: the authority for
  `zones`, `uspace_airspace` and `ussp_list`, the ANSP for
  `restrictions` (the CISP's `auth.PublisherOf`). The keys are
  `USSP_CIS_PUBLISHER_KEYS` (`authority=jwks_url,ansp=jwks_url`): the
  CISP does not serve the publishers' keys, its `/.well-known/jwks.json`
  holds its own signing keys only. A missing signature, one that does
  not verify, an `X-Publisher-Kid` that is not the signature's kid, or
  no keys (not configured, not fetched yet) holds the version: stored
  with `signature_ok` false, never used, never loaded by a warm start,
  never pruning the trusted version in use; `/readyz` `cis` is degraded
  with "... held, not used: <reason>" (so is the E-09 status line) and
  `cis_publisher_untrusted` counts it. The next pull checks it again; a
  version that cannot be read as published is a pull failure, not a
  hold. `/readyz` `cis_publisher_keys` says whether the keys are
  fetched.
- The signature age. Core's `DetachedConfig.MaxAge` is configurable
  (`DefaultDetachedMaxAge`, 5 min, applies only when it is zero). The
  publisher's iat is the publication time, and the CISP forwards that
  signature unchanged for the life of the version (what it caches per
  version is its own `X-CIS-Signature`, made at the first serve), so a
  signature is as old as its version. A USSP normally reads a version
  seconds after its publication, but a new installation or a lost
  database reads the current version however old it is.
  `USSP_CIS_PUBLISHER_SIG_MAX_AGE_S` defaults to 366 days (31 622 400 s;
  300 s to 10 years): a dataset left unpublished for a whole year (more
  than 13 AIRAC cycles) still verifies; an older one is held, visibly,
  and the bound can be raised. The iat bound is not the replay guard
  here: versions only move forward (one at or below the version held is
  never installed) and the CISP checks `body_sha256` before serving.
- What the signature does not cover (spec gap, for the CISP): the
  collection read from `GET /v1/{dataset}` is built by the CISP from its
  stored rows and is not compared with the signed bytes (for
  `restrictions` the signed bytes are the ANSP's request, not a
  collection). A version the CISP makes itself (a restriction expiring:
  publisher `system`) carries no publisher signature and is held until
  the ANSP's next signed version. The CISP's `X-CIS-Signature` is not
  verified: it is over the same version bytes. The pull is over TLS with
  a `cis.read` token.
- A `pull_url` is followed only when it is https with the scheme, host
  and port of `USSP_CISP_BASE_URL` (a missing port is the scheme's
  default) and carries no user information (`cis.Client.GetURL`).
  Plain http is never followed, not even on an http base URL (a lab
  CISP): the dataset is then read whole from the base URL.
- An issuer of `USSP_CIS_NOTIFY_ISSUERS` is the CISP or the ANSP by the
  host of its JWKS URL (the host of `USSP_CISP_BASE_URL` or
  `USSP_ANSP_BASE_URL`); one on neither refuses the start.
- `Age()`: the age is the time since the CISP last confirmed each ED-318
  dataset (a 200, a 304, or a 404 `no_version`, which makes the dataset
  a known empty one, `zones:0`); the oldest counts; nothing loaded is
  `("", 0, true)`. The `ussp_list` is cached but not part of the age.
- The token client of outgoing calls asks `POST /oauth/token` on the
  origin of the first `USSP_TOKEN_ISSUERS` entry's JWKS URL.
- The vector adapter maps the ED-269 zones of `zones_applicability.json`
  and `zones_vertical.json` to ED-318 with core's `ed318.FromED269`; a
  zone with no authority gets a placeholder (ED-318 requires one; no
  judgement reads it). The height-limit cases run through
  `Evaluator.JudgeHeightLimit` (a U-space airspace's
  `max_height_agl_m`). The `to_ed269` and `from_ed269` cases of
  `ed318_roundtrip.json` test a mapping the USSP never makes; for them
  the ED-318 side is ingested whole. 103 cases owned by ussp run.
- The KV value per cell (`CellEntry`) carries `zone/applicable/v1` bodies
  evaluated when projected, with the dataset, the restriction state and
  the feature, under `c5:` keys and `all` for a zone over 400 cells.
