# uspace-ussp

Reference U-space Service Provider (USSP) of the Georgian U-space
system-of-systems, on the EU model (Reg. (EU) 2021/664 Art. 7–13, 15;
ASTM F3411-22a, ASTM F3548-21, EUROCAE ED-318). One of possibly many
USSPs: third parties may write their own, and every interface this
system has is a standard one or a published national OpenAPI contract.

What it provides to UAS operators and to the ecosystem:

| Service | Standard or contract |
|---|---|
| Operator accounts and machine clients | own OAuth2 issuer (RS256, JWKS) |
| Network identification (Art. 8) | F3411-22a Net-RID Service Provider: ISA in the DSS, `GET /uss/flights`, `/details` |
| Flight authorisation (Art. 10) with strategic deconfliction | national operator API (`api/openapi.yaml`) + F3548-21 operational intents via an InterUSS DSS |
| Conformance monitoring (Art. 13) | alerts to the operator and nearby operators; F3548 `Nonconforming`/`Contingent` to peers; Annex V notice to the ANSP |
| Traffic information (Art. 11) | manned (ANSP feed, own e-conspicuity receiver) and unmanned (peers via F3411) traffic with trust class and age; CPA proximity alerts (national addition) |
| Geo-awareness (Art. 9) | the CISP's ED-318 publication, cached and versioned |
| Weather (Art. 12, optional) | configured source |
| Records (Art. 15(1)(g)), occurrences (376/2014), operating status (Art. 7(6)) | national API to the authority |
| Operator portal and USSP console | Next.js under `web/`, `ka`/`en` |

It is an observer and a service: **nothing here ever commands an
aircraft**, and it informs rather than resolves.

## Status

Planning. `docs/PLAN.md` is the implementation plan (architecture,
data model, published API, bus events, security, performance budgets,
testing, deployment, milestones S-M0..S-M6, 21 work packages in 7 waves,
open questions). Implementation starts with `docs/WORKPACKAGES/WP-0.md`.

## Layout (planned)

```
cmd/            api, telemetry-ingest, rid-sp, monitor, traffic-ws, dss-sync, tsdb-writer (+ ussp-dev)
internal/       domain packages shared by the processes (see docs/PLAN.md §4)
api/            openapi.yaml (the published national API) and the pinned F3411/F3548 files
schemas/        JSON Schemas this system produces (telemetry, alert, intent, coordination, traffic product)
migrations/     relational/ (PostgreSQL + PostGIS) and timeseries/ (TimescaleDB), goose, never merged
web/            operator portal and USSP console (Next.js, uspace-ui)
deploy/         Dockerfile, compose, Caddy snippet, ENV reference, conformance profile
testdata/       conformance and deconfliction vectors
docs/           PLAN.md, WORKPACKAGES/, RUNBOOKS/
```

## Links

- Plan: [`docs/PLAN.md`](docs/PLAN.md); work packages: [`docs/WORKPACKAGES/`](docs/WORKPACKAGES/)
- Rules for contributors and agents: [`CLAUDE.md`](CLAUDE.md)
- System spec, lessons, vectors and scenarios: `rootxkit/uspace-lab` (`docs/spec/`, `knowledge/`)
- Shared judgement library: `rootxkit/uspace-core` (pinned by tag)
- Shared UI kit: `rootxkit/uspace-ui`
- Siblings: `uspace-authority` (competent authority), `uspace-cisp` (common information service), `uspace-ansp` (ANSP interface)

Go 1.27, PostgreSQL 16 + PostGIS 3.4, TimescaleDB, NATS JetStream,
Next.js. Module path `github.com/rootxkit/uspace-ussp`.
