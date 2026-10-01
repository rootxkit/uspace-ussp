# WP-5: registry validity

Branch `feat/WP-5-registry-validity`. Milestone S-M1. Owns
`internal/registry/`, the endpoint `GET /v1/registry/validate`, the table
`registry_validity` (queries), KV `registry_validity`, the identification
adapter for our own flights (`core/identify` over the cache). Depends on
WP-1. Consumers: WP-2 (account validation), WP-7 (intent gate), WP-8
(identification on telemetry), WP-9 (`/uss/flights/{id}/details`).

## Read first

1. `docs/PLAN.md §4` (`registry`), `§5.1` (`registry_validity`), `§7`
   (KV), `§8` (PII minimum).
2. Spec `02 F8` in full (request, response, scopes, cache TTLs, the
   change feed, failure: `unknown` blocks a new authorisation inside
   U-space airspace and marks a flight `identification: unknown_operator`
   elsewhere), `02 F5` registration lookup row, `04 §3.2` (statuses,
   reasons, `basis`), `06 §5`.
3. `uspace-core/identify` (`Lookup`, `Snapshot`, `ResolveBound`,
   `ResolveBroadcast`, `Unavailable`, `SerialConflict`), `regnum`
   (`CompareKey`, `Validate`), `serial` (`FoldKey`); vectors
   `identification_status.json`, `serials_and_registration.json`,
   `fleet_match.json` (cases owned by `ussp`).
4. LESSONS G-01, G-04, G-05, G-08 (a projection is never an authority;
   write it with the change), G-10, G-11, I-05, SC-17.

## What to build

- `registry.Client`: `GET /v1/registry/validate?operator=&serial=&pilot=`,
  `POST /v1/registry/validate` (batch), `GET /v1/registry/changes?since=`
  on the authority with scope `registry.validate` and purpose header
  (`authorisation` or `identification`); body cap, deadline 2 s; built
  generated from the pinned copy `api/clients/authority.yaml` (`SOURCE`
  commit, CI diff; reconciliation M11), with `aud` = the authority's
  host (M18).
- `registry.Cache`: `Validate(ctx, keys, purpose) (Answers, error)`:
  cache hit within TTL (24 h positive, 5 min negative, from policy) →
  answer with `cache_age_s`; miss → client; client failure → `unknown`
  with `registry_unavailable` and a counter, never an error to the
  caller's judgement (the caller decides what `unknown` means). Writes go
  to `registry_validity` and to KV in one step (G-08).
- `registry.Feed`: polls `/changes?since=` every 30 s, invalidates the
  named keys (deletes rows and KV entries), counts.
- `registry.Lookup`: an `identify.Lookup` over the KV projection for the
  hot path (telemetry-ingest, rid-sp) and over the table for `api`:
  `UASBySerial` with fold-key matching (`MatchExact`, `MatchFolded`,
  `MatchAmbiguous`, `MatchNone` as G-05 says), `Operator`. A missing
  projection yields `identify.Unavailable` (reason
  `registry_unavailable`), never `unidentified`.
- `GET /v1/registry/validate` for operators (scope `ussp.intents`): the
  cached answer, status only, audited with purpose.
- `/readyz` entry `registry` with `last_success_age_s` and the feed's
  cursor age.

## Done when

- [ ] `identification_status.json` and `serials_and_registration.json`
  cases owned by `ussp` pass through `registry.Lookup` + `core/identify`
  (`RunOwned`); the `fleet_match.json` cases pass through the adapter WP-8
  will call (write the adapter here as `registry.FleetInput` mapping).
- [ ] TTLs both ways: positive answer served at 23 h, refetched at 25 h;
  negative at 4 min served, 6 min refetched. The change feed invalidates
  a key and the next lookup refetches (SC-17 step 2 in unit form).
- [ ] Fake authority down: cached answer with `cache_age_s`; uncached →
  `unknown` + `registry_unavailable`, counted; back up → fresh answer
  (E-02 both ways).
- [ ] A lookup response containing a name field (the fake authority can
  be told to misbehave) is refused and counted `registry_pii_refused`:
  the USSP never stores a field it did not ask for.
- [ ] Lint, race, coverage ≥ 85 %; `CHANGELOG.md`; PR with outputs.

## Safety notes

- `unknown` is a status with a reason, not an error. What it blocks
  (a new authorisation inside U-space airspace) is WP-7's decision table;
  what it marks (`unknown_operator` on a flight) is WP-8's. This WP only
  answers honestly.
- Compare registration numbers by `regnum.CompareKey` (public part,
  case-insensitive) and serials by `serial.FoldKey` only when unique
  (G-04, G-05). Do not write a comparison by hand.

## Commits

`feat(registry): F8 client with purpose and body bounds [WP-5 S-M1]`,
`feat(registry): validity cache with TTLs, change feed and KV projection [WP-5 S-M1]`,
`feat(registry): identify.Lookup over the projection [WP-5 S-M1]`,
`test(registry): run the owned identification vectors [WP-5 S-M1]`.
