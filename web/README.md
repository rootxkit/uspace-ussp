# uspace-ussp web

The operator portal and the USSP console (Next.js, App Router,
TypeScript strict, Tailwind). WP-0 bootstraps the shell: one page `/`
that renders the readiness of the API process in Georgian and English.

```
pnpm install --frozen-lockfile
pnpm gen:api        # src/api/types.ts from ../api/openapi.yaml (never edit it)
pnpm lint && pnpm typecheck
pnpm build && pnpm test:e2e   # Playwright smoke (npx playwright install chromium once)
USSP_WEB_API_URL=http://127.0.0.1:8080 pnpm dev
```

Rules (CLAUDE.md): `web/` renders only. It has no database, no NATS
client and no geometry library (ESLint `no-restricted-imports`); API
types come only from `openapi-typescript` (`src/api/` holds nothing
else, enforced by ESLint); every user-facing string is in both
catalogues of `src/i18n/` (`ka.ts` is typed by `en.ts`, so a missing
key fails `tsc`); Noto Sans Georgian is bundled through
`next/font/local` from the pinned `@fontsource/noto-sans-georgian`, and
the page makes no third-party request (the smoke test checks it).

`USSP_WEB_API_URL` is the BFF target, read on the server only (default
`http://127.0.0.1:8080`).

// KIT: pending uspace-ui

The shared kit `@rootxkit/uspace-ui` (theme, map, symbology, CSP, the
BFF and session helpers) is not on npmjs yet. It is added with an exact
pin when it publishes `0.1.0-rc` (reconciliation M32), never from a git
tag; the session cookie, the CSRF header and the kit's CSP arrive with
it and with WP-2.
