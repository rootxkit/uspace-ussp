import { defineConfig, devices } from "@playwright/test";

// The smoke test runs against the standalone production build (`pnpm
// build` first), served by e2e/serve.mjs as the image serves it, twice:
// once with a stub API whose /readyz answers (the page reads and shows
// it), once with the API unreachable (the page must say so rather than
// show nothing).
export const withAPI = "http://127.0.0.1:3100";
export const withoutAPI = "http://127.0.0.1:3102";
const stubAPI = "http://127.0.0.1:3101";

const app = (url: string, apiURL: string) => ({
  command: "node e2e/serve.mjs",
  url: `${url}/`,
  env: { PORT: new URL(url).port, HOSTNAME: "127.0.0.1", USSP_WEB_API_URL: apiURL },
  reuseExistingServer: false,
  timeout: 60_000,
});

export default defineConfig({
  testDir: "e2e",
  forbidOnly: !!process.env.CI,
  retries: 0,
  reporter: process.env.CI ? [["list"], ["html", { open: "never" }]] : "list",
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
  webServer: [
    { command: "node e2e/stub-api.mjs", url: `${stubAPI}/healthz`, env: { PORT: "3101" }, reuseExistingServer: false },
    app(withAPI, stubAPI),
    app(withoutAPI, "http://127.0.0.1:1"),
  ],
});
