# WP-3: standards code generation

Branch `feat/WP-3-standards-codegen`. Milestone S-M2 (needed by WP-9 and
WP-13). Owns `api/standards/` (the pinned OpenAPI files and `SOURCE`
records), `internal/stdapi/` (generated servers and clients plus the
boundary converters), `scripts/check-generated.sh` (extended), and the
501 stubs that wire the USS endpoints into `cmd/api` and `cmd/rid-sp`.
Depends on WP-0. Consumers: WP-9 (`rid-sp`), WP-13 (`dss-sync`, F3548
USS endpoints), WP-14 (peer polling).

## Read first

1. `docs/PLAN.md §2` D3, `§6.2`, `§15` Q5.
2. `uspace-core/f3411/SOURCE`, `f3548/SOURCE`, `generate.go`,
   `oapi-codegen.yaml`, `internal/oapialias` (how core turned one-element
   `anyOf` wrappers into aliases), `doc.go` of both; `f3411.UnmarshalRIDFlight`,
   `UnmarshalGetFlightsResponse`, `f3548.UnmarshalOperationalIntent`,
   `Altitude.HAEM`, `Volume4DToZonesEnvelope`, the `Scope` constants.
3. Spec `02 F6`, `F7` (every endpoint and constant), `09 §1.4`–`1.6`.
4. LESSONS E-03 (nothing on the wire from memory), E-05 (record tool
   versions), Z-03 (pin field names to sources you can cite).

## What to build

- `api/standards/f3411-v22a.yaml` and `f3548-v21.yaml`: byte-for-byte
  copies of the files core's `SOURCE` names, at the same commits, with
  `api/standards/SOURCE` recording repo, commit, path, URL, SHA-256,
  title, generator version. `scripts/fetch-standards.sh` re-fetches and
  checks the SHA-256; CI never fetches (offline check only).
- `internal/stdapi/f3411`: oapi-codegen v2 (pinned in `go.mod`'s tool
  directive at the version core used, v2.8.0) generating a **strict
  server** interface for the USS-side operations (`SearchFlights`,
  `GetFlightDetails`, `PostIdentificationServiceArea`) and a **client**
  for the DSS-side operations (`SearchIdentificationServiceAreas`,
  `GetIdentificationServiceArea`, `CreateIdentificationServiceArea`,
  `UpdateIdentificationServiceArea`, `DeleteIdentificationServiceArea`,
  `CreateSubscription`, `UpdateSubscription`, `DeleteSubscription`,
  `GetSubscription`, `SearchSubscriptions`), selected with
  `include-operation-ids`; types generated once per package with the same
  `oapialias` post-processing as core. Core's post-processor is under its
  `internal/`, so it cannot be imported: copy the small program into
  `internal/stdapi/oapialias` with its origin (repo, path, commit) in the
  file header, and keep it byte-identical to core's.
- `internal/stdapi/f3548`: strict server for the USS-side operations
  (operational intent details, telemetry, operational intent and
  constraint change notifications, USS report, log set; take the exact
  operation ids from the YAML, never from this brief); client for every
  `/dss/v1/*` operation PLAN §6.2 lists.
- `internal/stdapi/convert`: the one place that maps generated wire
  structs to core's validated types and back: `RIDFlightFromWire(b []byte)
  (*f3411.RIDFlight, error)` through core's `UnmarshalRIDFlight` (so the
  size bound and field checks are core's), `OperationalIntentFromWire`,
  `Volume4DEnvelope` (core's), and the reverse writers. A test that
  round-trips core's `testdata/examples` through the generated types.
- Wiring: `cmd/api` mounts the F3548 strict server with every handler
  returning 501 problem+json `not_implemented` (WP-13 replaces them);
  `cmd/rid-sp` mounts the F3411 USS server likewise (WP-9 replaces). The
  scope middleware from WP-2 is applied with the standard's scopes
  (`rid.display_provider` on `/uss/flights*`, `rid.service_provider` on
  the ISA notification, `utm.strategic_coordination` /
  `utm.conformance_monitoring_sa` / `utm.constraint_processing` as the
  file says per operation; read the `security` blocks, do not guess).
- `scripts/check-generated.sh` extended to regenerate both packages into
  a temp dir and diff, and to verify `SOURCE` SHA-256s against the
  vendored files.

## Done when

- [ ] Generated code committed; `check-generated.sh` passes offline in CI
  and fails when a generated file is edited by hand (prove it in a test
  run, paste the failure, revert).
- [ ] Every USS-side operation of `02 §3 ussp` and `02 F6` is mounted
  (a test lists the router's patterns and compares with the plan's list,
  both directions).
- [ ] Scope per operation matches the OpenAPI `security` block (a test
  reads the YAML and the middleware table).
- [ ] Round-trip of core's example messages through `convert` equal by
  value.
- [ ] No hand-written F3411/F3548 struct anywhere (`grep -rn "json:\"uss_base_url\""`
  outside generated files finds nothing); lint, race; `CHANGELOG.md`;
  PR with outputs including the generator version and SHA-256s (E-05).

## Safety notes

- A field name on the wire comes from the file; when the file and the
  spec text disagree, the file wins and the disagreement goes into the PR
  and `docs/PLAN.md §15`.
- The 501 stubs must carry the right scope checks now: an unauthenticated
  501 would teach a peer that the endpoint is open.

## Commits

`build(stdapi): pin the F3411 v22a and F3548 v21 OpenAPI files [WP-3 S-M2]`,
`feat(stdapi): generate USS servers and DSS clients [WP-3 S-M2]`,
`feat(stdapi): boundary converters through core's validators [WP-3 S-M2]`,
`feat(api): mount the standard endpoints with scopes and 501 [WP-3 S-M2]`.
