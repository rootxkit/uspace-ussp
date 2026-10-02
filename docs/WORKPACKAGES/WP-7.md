# WP-7: flight authorisation (intents)

Branch `feat/WP-7-intent-authorisation`. Milestone **S-M1**. Owns
`internal/intent/` and `internal/intent/deconflict/`, the endpoints
`/v1/intents/*`, the `intent.v1` subject and KV `intent_active`, the
schemas `intent/request/v1`, `intent/decision/v1`, `intent/state/v1`
under `schemas/`, `testdata/vectors/deconfliction.json`. Depends on WP-2
(scopes, accounts), WP-4 (`cis.Evaluator`), WP-5 (`registry.Cache`), WP-6
(bus). May start against those packages' signatures the day their PRs are
open. Consumers: WP-10 (volumes and thresholds via `intent_active`),
WP-12 (standing re-check), WP-13 (DSS write), WP-17 (portal).

This WP is the critical path of S-M1 and a safety-relevant review: it
decides what flies where. Split into `WP-7a` (intake, validation,
decision, states, API) and `WP-7b` (deconfliction package and vectors)
if one PR grows past review size; say so in the PR.

## Read first

1. `docs/PLAN.md §1` (what the USSP does not do: require authorisation
   outside Art. 1(3) scope; grant one that conflicts), `§2` D2, D10,
   `§5.1` (`operational_intents`, `intent_versions`), `§6.1`, `§7`,
   `§15` Q1, Q6, Q7, Q16, Q17.
2. Spec `01 §3` S3 in full and the MUST NOT list, `02 F5` (operational
   intent row: states, DSS down behaviour, standing authorisation
   updates), `03 §3` (`operational_intents` columns, every one),
   `03 §6` (authorisation number format), `04 §3.5` (the ten Annex IV
   items by number, `intent/decision/v1` fields), `09 §1.1` rows Art. 6(4)
   through 10(11), Annex IV, `09 §1.6` (F3548 `Priority`, `OiMaxPlanHorizonDays`,
   Volume4D references).
3. `uspace-core/f3548` doc.go, `Volume4D`, `Volume3D`, `Altitude.HAEM`,
   `Volume4DToZonesEnvelope`, `UnmarshalOperationalIntent` (use it on our
   own request body's volumes: they are F3548 `Volume4D`), `DSSStates`;
   `core/geodesy` (`Polygon.Contains`, `Circle`, `BBox`, `ValidRing`),
   `core/geoid` (`Undulator`, `AMSLFromHAE`), `core/zones.Severity`.
4. LESSONS INV-03, E-01, E-15 (a zero or invalid threshold refuses the
   check, never disarms it), D-01 (AMSL for separation; W84 kept as
   given), Z-06 (bound what an import can cost), X-11 (what came back
   from the courier scope as a service, not a planner).
5. Reference only: utm `TASKS.md` U-05 done-when (either order, same
   result; refusal names the item).

## What to build

- `schemas/intent/request/v1.json`, `decision/v1.json`, `state/v1.json`
  (JSON Schema 2020-12, `$id` as `04 §1`), with examples; Go structs
  generated (`go-jsonschema` as a `go run` tool) or hand-written with a
  round-trip test against the examples (either way `check-schemas.sh`
  proves the examples validate and round-trip).
- `intent.Validate(req) (Normalised, ed269.Problems)`: the ten Annex IV
  items present and well-formed (numbered problems `annex_iv.1` ...
  `annex_iv.10` so the operator sees which item), `volumes` through core's
  F3548 validation, planning horizon ≤ 30 days, `time_end > time_start`,
  endurance covers the window, serial via `core/serial.ValidateForClass`,
  registration numbers via `core/regnum`, `priority` only 0 or the
  configured special-operation class (`policy.SpecialOperationPriority`,
  default 100) and only with `flight_type = special_operation`; volumes
  → AMSL per volume via the geoid (`lower_amsl_m`, `upper_amsl_m`,
  `undulation_m` at the outline centroid; a missing geoid refuses the
  intent with `geoid_unavailable`, never approximates); envelope
  geography.
- `intent.Decide(ctx, n Normalised) Decision` in `api`'s request path,
  in this order, each step recorded in `conflicts[]`/`conditions[]`:
  1. **Scope**: `exempt_art_1_3` when category open, subcategory A1,
     class C0 or privately built < 250 g (from the request's class and
     MTOM declaration) → the intent is accepted without an authorisation
     number (`decision: accepted_voluntary`) and skips steps 3–6; it still
     gets a flight and network identification.
  2. **Registry** (S8): operator, serial, pilot through `registry.Cache`
     with purpose `authorisation`; `suspended`/`revoked` → rejected with
     the item; `unknown` → `pending_validation` if any volume is inside
     U-space airspace, else a condition `registry_unverified` on an
     accepted intent (02 F8 failure rule).
  3. **Airspace**: `cis.Evaluator.AirspacesAt` over the volumes →
     `in_uspace_airspace`, `uspace_airspace_ids`; the airspace's
     `airspace_constraints` (height ceiling, operational conditions) cap
     the volumes: a volume above the ceiling is rejected naming the
     airspace; `cis.Age()` stale beyond bound and inside U-space airspace
     → rejected `cis_stale` (02 F3).
  4. **Zones and restrictions** (Art. 10(7)): `cis.Evaluator.ZonesFor`
     over the envelope and window; a `PROHIBITED` zone or an active DAR
     restriction overlapping in space (horizontal containment of any
     outline vertex or outline–polygon intersection via `geodesy`,
     vertical band overlap in AMSL with the zone's limits converted
     through the geoid where WGS84 and through terrain where AGL — a
     limit that cannot be judged counts as overlapping, Z-09 spirit) and
     time → rejected naming the zone id and the overlap; `REQ_AUTHORIZATION`
     → `pending_authority` unless `authorisation_ref` is given, in which
     case a condition; `CONDITIONAL` → condition with the zone's message.
  5. **Deconfliction** (Art. 10(2)(b), (8), (9)): `deconflict.Check(mine,
     others)` over local intents in states accepted/activated/nonconforming/
     contingent whose envelope and window overlap (store query), plus
     `peer_intents` within 24 h (WP-13 fills the table; an empty table is
     fine). Rules in `deconflict`: a conflict is overlap in horizontal
     (polygon–polygon / circle via `geodesy`, with a configurable buffer
     `policy.DeconflictBufferM`, default 0), vertical (AMSL bands with
     `policy.DeconflictVerticalBufferM`, default 0) and time (closed
     intervals); a higher `priority` wins over a lower one (the lower
     is rejected or, if already accepted, flagged for update per Art.
     10(10) — WP-12 does the update); equal priority → first come first
     served by `created_at` then `id` (deterministic: filing A then B or
     B then A gives the same pair decision).
  6. **Decision**: accepted → `authorisation_number =
     <USSP_SYSTEM_ID>-<operator public reg>-<ULID>` (`03 §6`),
     `deviation_thresholds` from policy (per-airspace override from the
     airspace's `service_performance` when present), `alternative`
     optional (first implementation: none; field present and null),
     `local_state = accepted`, `dss_state = Accepted` locally pending
     WP-13's write (`pending_dss` when any volume is inside U-space
     airspace and the DSS write is required and unavailable; outside
     U-space airspace local checks suffice, 02 F5); `cis_version_checked`,
     `registry_checked_at`, `policy_version`, `weather_checked_ref` (null
     until WP-16).
- States and transitions (`local_state`), each an `intent_versions` row
  and an `intent.v1.<state>.<id>` message: `pending_validation`,
  `pending_dss`, `pending_authority`, `accepted`, `activated` (`PATCH
  action=activate`: confirmed in the response; refused if before
  `time_start - policy.ActivationLeadS` or after `time_end`), `nonconforming`,
  `contingent` (set by WP-10 through an internal call), `ended` (`PATCH
  action=end`, or `time_end` passed, or the flight ended), `rejected`,
  `withdrawn` (by WP-12's re-check with `change_reason`). `modify` =
  new volumes re-decided as a new version keeping the id and the
  authorisation number when still accepted (Art. 6(6)).
- KV `intent_active`: every intent in accepted/activated/nonconforming/
  contingent with volumes AMSL, thresholds, `flight_id` (null until WP-8
  binds), cell set, state; written with each transition (G-08).
- Endpoints of PLAN §6.1 (`POST`, `GET`, list, `PATCH`), scope
  `ussp.intents`, operator ownership enforced, idempotent on
  `(client_id, client_ref)` (same body → same decision, 200; different
  body → 409), `intent/decision/v1` responses, audit rows with purpose.
- `testdata/vectors/deconfliction.json` in the lab's file shape: ≥ 30
  cases: disjoint in space, time, altitude; touching bands; polygon vs
  circle; antimeridian-safe; priority win; FCFS either order; buffer
  on/off; invalid threshold refuses (E-15); U-space ceiling cap; Art.
  1(3) exemption; `cis_stale`; registry `unknown` inside and outside
  U-space airspace. Each with `why`.

## Done when

- [ ] S-M1 done-when of PLAN §12 reproduced in the integration test
  with the fakes (every clause, each a named test): ten items; CIS
  checks; two overlapping intents in either order → same decision;
  special operation wins; equal priority FCFS; refusal names the item;
  acceptance carries number and thresholds; activation confirmed;
  registry fetched and cached; C0 A1 accepted without authorisation.
- [ ] `deconfliction.json` passes; the harness is `core/vectors` on the
  local file (`vectors.Read(path)`).
- [ ] E-01 pairs for every rejection and every pending state; E-02: the
  success path's response body read and compared field by field with
  the schema example.
- [ ] Decision latency p95 ≤ 500 ms with 1000 active local intents in
  the integration test (print it).
- [ ] Lint, race, coverage ≥ 85 % on `intent` and `deconflict`;
  `CHANGELOG.md`; PR with outputs and the vector count.

## Safety notes

- Never grant an authorisation that conflicts with a zone, a restriction
  or an existing intent, whoever asks (01 §3 MUST NOT). There is no
  override flag; a supervisor's path to force an authorisation does not
  exist in this plan.
- Thresholds, buffers and priorities come from `policy`; `policy_version`
  is on the decision. A test that relaxes a threshold to pass is a review
  failure (INV-03).
- A limit or input that cannot be judged (no geoid, no terrain, stale
  CIS) refuses or holds; it never passes by default (E-15, Z-09).
- Nothing here sends anything to an aircraft or a GCS beyond the HTTP
  response to the operator's request.

## Commits

`feat(intent): Annex IV validation and AMSL volumes [WP-7 S-M1]`,
`feat(deconflict): space, time and altitude overlap with priority [WP-7 S-M1]`,
`feat(intent): decision pipeline with zones, registry and airspace [WP-7 S-M1]`,
`feat(intent): states, versions, authorisation number and the API [WP-7 S-M1]`,
`test(deconflict): the deconfliction vectors [WP-7 S-M1]`.

## As built (the PR)

What the build decided where the brief, the plan or core left a choice,
and where it is stricter than the brief; each is in the PR body as well.

- **Nothing untrusted, stale or unavailable authorises.** A stale CIS
  refuses everywhere, not only inside U-space airspace (whether a volume
  is outside it is read from the stale cache). A newer CIS version held
  untrusted, refused, or notified and not pulled refuses with
  `cis_outdated` (`cis.Cache.Outdated`, added here). A registry answer
  that is `unknown` (not held, unavailable or refused) holds the intent
  `pending_validation` inside **and outside** U-space airspace; the brief
  asked for an accepted intent with `registry_unverified` outside. No
  geoid refuses with 503 `geoid_unavailable`.
- **Exempt (Art. 1(3)) intents** skip deconfliction, the DSS and the
  number, but not the CIS checks: a C0 A1 volume over a PROHIBITED zone
  or an active restriction is refused (the brief skips steps 3 to 6).
  Exempt intents are never in another intent's deconfliction set.
- **No DSS writer before WP-13.** The api process's DSS is never
  available, so an intent inside U-space airspace waits as
  `pending_dss`; outside, local checks suffice (02 F5). Pending intents
  have no path to accepted in this WP (WP-12/13 re-decide them); they
  can be ended.
- **AGL is not judged over an area yet.** uspace-core has no lowest or
  highest ground under an outline, so the api process runs without
  terrain: a zone limit in AGL counts as overlapping (Z-09) and a
  U-space airspace with `max_height_agl_m` refuses with
  `airspace_ceiling_not_judged`. The `Terrain` interface is there for
  the day core offers an area range. WGS84 zone limits are compared with
  the volume's W84 band as given; AMSL limits with the AMSL band.
- U-space airspace membership is the volume's outline against the
  airspace's (deconflict geometry), not `AirspacesAt` at a point, so a
  volume that only grazes an airspace is inside it. Zone holes are not
  subtracted (more refusals, never fewer).
- Geometry: edges are straight in latitude and longitude as core's
  `geodesy.Polygon` reads them; separations are Vincenty distances to
  the nearest point found on the vertex's tangent plane; a pair within
  1 cm (F3548 `IntersectionMinimumPrecisionCm`) plus 1e-4 of the
  separation of the buffer is a conflict.
- First come, first served ranks on `filed_at`, when the volumes judged
  were filed: a modification re-ranks (an operator cannot file early and
  then move into others' space). Equal instants fall back to the id.
  Peers' intents rank as filed when fetched (so they precede ours at
  equal priority); a peer intent that cannot be judged refuses the
  decision with 503.
- `special_operation` is self-declared by the operator (spec gap: the
  regulation's special operations should be verified by the authority);
  the priority follows the flight type and a stated priority must equal
  it.
- Modify is accepted only in `accepted` (not in flight); a modification
  that is not authorised leaves the intent in its new decision's state
  without its number.
- `operator_reg` is checked as the operator account stores it (text
  without white space, compared on its public part with regnum); the
  format is the registry's. The client must hold a live binding of the
  serial (403 `serial_not_bound`) and `operator_reg` must be its
  operator's (403 `operator_mismatch`).
- Schemas follow uspace-lab's layout
  (`schemas/<name>/v1/schema.json`, `examples/`, `examples/invalid/`) so
  the lab mirrors the directory byte for byte; PLAN §4 named
  `schemas/examples/`. `check-schemas.sh` gained its validator here
  (`schemas/schemas_test.go`, santhosh-tekuri/jsonschema already in
  go.mod) since WP-7's schemas are the first.
- The KV put and the `intent.v1` publish run inside the transaction
  before it commits (B-09: either failing refuses with 503 and nothing is
  written); a commit that fails after them leaves a projection without a
  row until the next transition of that id.
- The lab scenario is run in process (docs/RUNBOOKS/WP-7.md): the lab has
  no runner for this USSP's intents yet.

