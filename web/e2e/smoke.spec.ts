import { expect, test } from "@playwright/test";

import { withAPI, withoutAPI } from "../playwright.config";

// The one page in both languages, with the API answering (the page
// reads /readyz and shows every dependency) and with the API away (the
// page says it did not answer: an unreachable input is shown as such,
// never as an empty list; CLAUDE.md rule 7).
const langs = [
  { lang: "en", heading: "Service readiness", notReady: "Not ready", down: "Down", unreachable: "The API did not answer" },
  { lang: "ka", heading: "სერვისის მზადყოფნა", notReady: "არ არის მზად", down: "მიუწვდომელია", unreachable: "API არ პასუხობს" },
] as const;

for (const l of langs) {
  test(`renders the readiness of the API in ${l.lang}`, async ({ page }) => {
    const response = await page.goto(`${withAPI}/?lang=${l.lang}`);
    expect(response?.status()).toBe(200);
    await expect(page.locator("main")).toHaveAttribute("lang", l.lang);
    await expect(page.getByRole("heading", { level: 2 })).toHaveText(l.heading);
    await expect(page.getByTestId("readiness-status")).toContainText(`${l.notReady} (HTTP 503)`);
    await expect(page.getByTestId("dependency-nats")).toContainText(l.down);
    await expect(page.getByTestId("dependency-nats")).toContainText("not connected (RECONNECTING)");
    await expect(page.getByTestId("dependency-postgres")).toBeVisible();
    await expect(page.getByTestId("readiness-unreachable")).toHaveCount(0);
  });

  test(`says the API is unreachable in ${l.lang}`, async ({ page }) => {
    const response = await page.goto(`${withoutAPI}/?lang=${l.lang}`);
    expect(response?.status()).toBe(200);
    await expect(page.getByTestId("readiness-unreachable")).toContainText(l.unreachable);
    await expect(page.locator("table")).toHaveCount(0);
  });
}

test("Accept-Language picks the language when ?lang is absent", async ({ browser }) => {
  const context = await browser.newContext({ locale: "en-GB" });
  const page = await context.newPage();
  await page.goto(`${withAPI}/`);
  await expect(page.locator("main")).toHaveAttribute("lang", "en");
  await context.close();
});

test("no third-party request", async ({ page }) => {
  const origins = new Set<string>();
  page.on("request", (req) => origins.add(new URL(req.url()).origin));
  await page.goto(`${withAPI}/?lang=ka`);
  await page.waitForLoadState("networkidle");
  expect([...origins].filter((o) => o !== new URL(withAPI).origin)).toEqual([]);
});
