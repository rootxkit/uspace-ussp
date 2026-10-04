import { defineConfig, devices } from "@playwright/test";

// The portal's browser tests (brief WP-17) run against the standalone
// production build (`pnpm build` first), served by e2e/serve.mjs as the
// image serves it, behind the e2e stack of test/e2e/stack: the seven
// processes of cmd/ussp-dev on the real PostgreSQL, TimescaleDB and NATS
// (USSP_TEST_PG_URL, USSP_TEST_TS_OWNER_URL, USSP_TEST_NATS_URL) with
// the fakes of internal/testfakes, and one same-origin front (the web
// app, and traffic-ws's WebSockets, as Caddy routes them). Every test
// records its trace; CI uploads them (the S-M1 run is the evidence of
// the brief's first done-when item).
export const FRONT = "http://127.0.0.1:3200";
export const CONTROL = "http://127.0.0.1:3290";
const WEB = "http://127.0.0.1:3100";
const API = "http://127.0.0.1:3301";

export default defineConfig({
  testDir: "e2e",
  forbidOnly: !!process.env.CI,
  retries: 0,
  workers: 1,
  timeout: 180_000,
  expect: { timeout: 20_000 },
  reporter: process.env.CI ? [["list"], ["html", { open: "never" }]] : "list",
  use: { baseURL: FRONT, trace: "on", screenshot: "on" },
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
  webServer: [
    {
      // From the repository root; reused when one is already running locally.
      command: "go run ./test/e2e/stack",
      cwd: "..",
      url: `${CONTROL}/healthz`,
      reuseExistingServer: !process.env.CI,
      timeout: 240_000,
      stdout: "pipe",
      stderr: "pipe",
    },
    {
      command: "node e2e/serve.mjs",
      url: `${WEB}/login`,
      // USSP_WEB_BFF_SECRET seals a staff admin's MFA challenge between
      // the console's two sign-in steps (brief WP-18): a test value.
      env: { PORT: "3100", HOSTNAME: "127.0.0.1", USSP_WEB_API_URL: API, USSP_WEB_SESSION_SECURE: "false", USSP_WEB_BFF_SECRET: "e2e-only-bff-secret-of-at-least-32-bytes" },
      reuseExistingServer: !process.env.CI,
      timeout: 60_000,
    },
  ],
});
