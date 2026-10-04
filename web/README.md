# uspace-ussp web

The operator portal of this USSP (brief WP-17; the staff console of
WP-18 joins it): Next.js App Router, TypeScript strict, Tailwind, on the
shared kit `@rootxkit/uspace-ui` (theme, fonts, map, layers, legends,
live client, form, BFF and session helpers, ESLint rules). It renders
what the API says and judges nothing.

```
pnpm install --frozen-lockfile
pnpm gen:api              # src/api/generated/openapi.d.ts from ../api/openapi.yaml (never edit it)
pnpm check:i18n           # en/ka complete, no duplicate key, equal placeholders, no manoeuvre advice
pnpm lint && pnpm typecheck
pnpm build
pnpm test:e2e             # Playwright against the e2e stack (below); pnpm exec playwright install chromium once
USSP_WEB_API_URL=http://127.0.0.1:8080 USSP_WEB_SESSION_SECURE=false pnpm dev
```

## The kit

`@rootxkit/uspace-ui` is the GitHub Release asset of `v0.1.0`, pinned by
URL in `package.json`; `pnpm-lock.yaml` records its sha512 integrity and
`--frozen-lockfile` refuses other bytes (uspace-ui `docs/CONSUMING.md`
§1). Its signed build provenance was checked before pinning:

```
gh release download v0.1.0 --repo rootxkit/uspace-ui --pattern 'rootxkit-uspace-ui-0.1.0.tgz'
gh attestation verify rootxkit-uspace-ui-0.1.0.tgz --repo rootxkit/uspace-ui \
  --signer-workflow rootxkit/uspace-ui/.github/workflows/release.yml
```

## Pages

| Path | What | API |
|---|---|---|
| `/register` | operator self-registration; the state the registry decided (status only) | `POST /v1/accounts/operators` |
| `/login` | the kit's `LoginForm` on `/_bff/login` (realm `portal`) | `POST /v1/accounts/login` |
| `/clients` | the operator's clients; create (secret shown once), rotate, bind and unbind serials | `/v1/accounts/operators/{id}/clients*` |
| `/intents`, `/intents/new`, `/intents/{id}` | the list with states; the ten Annex IV items with the outline clicked on the map or typed; the decision (number, thresholds, conflicts with the item named, conditions, AMSL derivation, versions); activate, modify, end | `/v1/intents*` |
| `/geo` | zones, U-space airspaces (requirements) and restrictions around the map or an intent, each with version, `updated_at` and validity; stale with its age; refetched on `geo/changed/v1` | `/v1/geo`, `/v1/geo/intents/{id}`, `WS /v1/traffic` |
| `/traffic?intent=` | the live product with the kit's symbology, every track with state and `age_s`, degraded inputs with their time, proximity alerts with numbers and the other aircraft, `dropped_frames` | `WS /v1/traffic` |
| `/alerts?intent=` | the intent's alerts with repeats and escalation; acknowledgement | `WS /v1/alerts`, `POST /v1/alerts/{id}/ack` |
| `/weather` | products for the planning box, or "not configured" / stale | `GET /v1/weather` |

## Rules

- The BFF is `/_bff/login`, `/_bff/logout`, `/_bff/api/*` and nothing
  else (`src/lib/bff/`): the session JWT in `uspace_session` (`HttpOnly;
  SameSite=Strict`, `Secure` unless `USSP_WEB_SESSION_SECURE=false`), the
  readable `uspace_csrf` as `X-CSRF-Token` on every unsafe request. The
  request proxy (`src/proxy.ts`) issues `uspace_csrf` to a visitor
  without one, so the self-registration passes the double submit. The
  BFF reaches only the paths of `src/lib/bff/paths.ts`.
- The browser opens `WS /v1/traffic` and `WS /v1/alerts` on its own
  origin with the session cookie; no ticket, no token in a URL (M22).
- The kit's ESLint config refuses a geometry or geodesy import, a
  database or bus client, anything but the BFF helpers in a route
  handler, and a hand-written file in `src/api/generated/`; this repo's
  list (`eslint.config.mjs`) adds its own names.
- Every string is in `src/i18n/en.json` and `ka.json` (`ka` by default,
  then the `uspace_lang` cookie and `Accept-Language`); a key missing in
  `ka` fails `tsc`, the rest `pnpm check:i18n`.
- The kit's CSP per request with a nonce (`connect-src 'self'`,
  `font-src 'self'`, `worker-src blob:`), fonts through `next/font/local`
  from the kit, the basemap at `/basemap/` from the deployment; the
  browser tests check that nothing leaves the origin.
- The kit 0.1.0 has no drawing tool (KIT: pending uspace-ui): an outline
  is points clicked on the map or typed; a circle is shown as its centre
  and radius in words (drawing it would need geometry here).

## Configuration (read at request time)

| Variable | Default | Meaning |
|---|---|---|
| `USSP_WEB_API_URL` | `http://127.0.0.1:8080` | the api process as this server reaches it (the BFF target) |
| `USSP_WEB_SESSION_SECURE` | `true` | `false` only for a plain-HTTP local run |
| `USSP_WEB_TRUSTED_PROXY_HOPS` | (none) | reverse proxies in front of Next.js (1 behind the deployment's Caddy); required with a secure session. List the web container in the API's `USSP_TRUSTED_PROXIES` |
| `USSP_WEB_BFF_TIMEOUT_MS` | `10000` (`config/portal.json`) | upstream timeout of every BFF call |
| `USSP_WEB_MAP_CENTER`, `USSP_WEB_MAP_ZOOM` | Tbilisi, 10 (`config/portal.json`, pending GCAA) | the maps' first view, `lng,lat` |
| `USSP_WEB_BASEMAP_ORIGIN` | (none) | build time, local runs only: where `/basemap/*` is rewritten to |
| `UI_BRAND_*` | the kit's | brand name, short name, logo, contact, accent |

## Browser tests

`pnpm test:e2e` builds nothing: run `pnpm build` first. Playwright
starts `go run ./test/e2e/stack` (from the repository root) and the
standalone server. The stack runs the seven processes against
`USSP_TEST_PG_URL`, `USSP_TEST_TS_OWNER_URL` and `USSP_TEST_NATS_URL`
(as `make integration` reads them) with the fakes of
`internal/testfakes` and `internal/dss/fakedss`, serves the browser one
origin on `127.0.0.1:3200` and a control listener on `127.0.0.1:3290`.
Every test records its trace and screenshots under `test-results/`;
`docs/RUNBOOKS/WP-17.md` records a run.
