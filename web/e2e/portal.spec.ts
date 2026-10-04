import AxeBuilder from "./axe";
import { expect, test, type Page } from "@playwright/test";
import { control, sameOrigin, unique, utcInput } from "./helpers";

// The operator flows of brief WP-17 in the browser, end to end against
// the e2e stack (playwright.config.ts): register, sign in, create a
// client (its secret shown once) and bind a serial, file an intent with
// its outline typed as points, read the decision, activate it, open the
// geo-awareness, follow the traffic of a flight the simulated operator
// client flies while a second operator's aircraft hovers beside it, see
// the proximity alert with its numbers and the other aircraft, and
// acknowledge it; in Georgian and in English. The browser makes no
// request off its origin.

// A place of this run's own inside the map's first view (Tbilisi, zoom
// 10), so an intent a previous run left in a shared database does not
// meet this one's (strategic deconfliction would rightly refuse it).
const TBILISI = { lat: 41.6 + Math.random() * 0.2, lng: 44.6 + Math.random() * 0.4 };
const D = 0.002;

/** A full-page screenshot in the test's output (CI uploads it with the trace). */
async function shot(page: Page, name: string) {
  await page.screenshot({ path: test.info().outputPath(`${name}.png`), fullPage: true });
}

async function fill(page: Page, label: RegExp, value: string) {
  await page.getByLabel(label).fill(value);
}

test.describe.configure({ mode: "serial" });

test("S-M1 and S-M3: register, client, intent, decision, geo, traffic, alert acknowledged", async ({ page, request }) => {
  const offOrigin = sameOrigin(page);
  const u = unique();
  const number = `GEO-TEST-E2E${u}`;
  const serial = `TESTE2E${u}`;
  const user = `pilot.${u}`;
  const pass = `portal-password-${u}`;
  await control(request, "/registry", { operator: number, serial });

  // Register (the fake registry says valid: active).
  await page.goto("/register");
  await expect(page.locator("html")).toHaveAttribute("lang", "en");
  await fill(page, /^Operator registration number/, number);
  await fill(page, /^Display name/, `E2E operator ${u}`);
  await fill(page, /^Contact email/, `ops${u}@example.test`);
  await fill(page, /^Username of the first portal user/, user);
  await fill(page, /^Password of the first portal user/, pass);
  await page.getByRole("button", { name: "Register" }).click();
  await expect(page.getByTestId("registered")).toHaveAttribute("data-status", "active");
  await expect(page.getByTestId("validation-status")).toContainText("valid");

  // Sign in through the BFF.
  await page.goto("/login");
  await page.getByLabel("Username").fill(user);
  await page.getByLabel("Password").fill(pass);
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page).toHaveURL(/\/intents$/);
  await expect(page.getByTestId("session")).toContainText("operator_admin");
  const cookies = await page.context().cookies();
  expect(cookies.find((c) => c.name === "uspace_session")?.httpOnly).toBe(true);
  expect(await page.evaluate(() => document.cookie)).not.toContain("uspace_session");

  // A client with every scope: the secret once; bind the serial.
  await page.goto("/clients");
  await page.getByRole("button", { name: "Create", exact: true }).click();
  const secretBox = page.getByTestId("client-secret");
  await expect(secretBox).toBeVisible();
  const clientId = (await page.getByTestId("secret-client-id").textContent()) ?? "";
  const clientSecret = (await page.getByTestId("secret-value").textContent()) ?? "";
  expect(clientId).toMatch(/^op-/);
  expect(clientSecret.length).toBeGreaterThan(20);
  await page.getByRole("button", { name: "I have copied it" }).click();
  await expect(secretBox).toHaveCount(0);
  const row = page.getByTestId(`client-${clientId}`);
  await row.getByText("Bind a serial").click();
  await row.getByLabel(/^UAS serial number/).fill(serial);
  await row.getByRole("button", { name: "Bind" }).click();
  await expect(row).toContainText(serial);
  await expect(page.getByText(clientSecret)).toHaveCount(0);

  // File an intent: the ten items, the outline typed as points.
  await page.goto("/intents/new");
  for (const [lat, lng] of [
    [TBILISI.lat - D, TBILISI.lng - D],
    [TBILISI.lat - D, TBILISI.lng + D],
    [TBILISI.lat + D, TBILISI.lng + D],
    [TBILISI.lat + D, TBILISI.lng - D],
  ] as const) {
    await page.getByLabel("Latitude (WGS84 degrees)").fill(String(lat));
    await page.getByLabel("Longitude (WGS84 degrees)").fill(String(lng));
    await page.getByRole("button", { name: "Add the point" }).click();
  }
  await expect(page.getByTestId("outline-points").locator("li")).toHaveCount(4);
  await fill(page, /^UAS serial number/, serial);
  await page.getByLabel(/^Category/).selectOption("specific");
  await fill(page, /^Lower altitude/, "600");
  await fill(page, /^Upper altitude/, "700");
  const start = new Date(Date.now() + 2 * 60_000);
  await fill(page, /^Start, UTC/, utcInput(start));
  await fill(page, /^End, UTC/, utcInput(new Date(start.getTime() + 45 * 60_000)));
  await fill(page, /^Connectivity methods/, "lte, radio");
  await fill(page, /^Endurance \(s\)/, "2700");
  await fill(page, /^Procedure on loss of command and control/, "return to the take-off point");
  await fill(page, /^Operator registration number/, number);
  await fill(page, /^Contingency measures/, "land at the nearest landing site");
  await fill(page, /^Emergency contact reference/, `EC-TEST-${u}`);
  await shot(page, "intent-form-en");
  await page.getByRole("button", { name: "File the intent" }).click();

  // The decision as the API answered it.
  await expect(page).toHaveURL(/\/intents\/[0-9a-f-]{36}$/);
  const intentId = page.url().split("/").pop() ?? "";
  const decision = page.getByTestId("decision");
  await expect(decision).toHaveAttribute("data-decision", "authorised");
  await expect(page.getByTestId("authorisation-number")).toContainText(`USSP-DEV-${number}-`);
  await expect(page.getByTestId("thresholds")).toContainText("m");
  await expect(page.getByTestId("volumes-amsl")).toContainText("580");
  await expect(page.getByTestId("no-conflicts")).toBeVisible();
  await shot(page, "decision-en");
  await page.getByRole("button", { name: "Activate" }).click();
  await expect(decision).toHaveAttribute("data-state", "activated");

  // Geo-awareness around the map: the CIS's zone with its version.
  await page.goto("/geo");
  const zone = page.locator('[data-testid="geo-item"][data-identifier="TZE2E01"]');
  await expect(zone).toBeVisible();
  await expect(zone).toContainText("zones:");
  await expect(page.getByTestId("geo-basis")).toContainText("uspace_airspace:");
  await expect(page.getByTestId("geo-stale")).toHaveCount(0);
  await shot(page, "geo-en");

  // The flight, and another operator's aircraft hovering 30 m east.
  await control(request, "/fly", { client_id: clientId, client_secret: clientSecret, serial, intent_id: intentId, lat: TBILISI.lat, lng: TBILISI.lng });
  await control(request, "/intruder", { lat: TBILISI.lat, lng: TBILISI.lng, east_m: 30 });
  await page.goto(`/traffic?intent=${intentId}`);
  await expect(page.getByTestId("feed")).toHaveAttribute("data-connection", "live");
  await expect(page.locator('[data-testid="track-row"][data-own="true"]')).toHaveCount(1);
  // The other operator's aircraft (and any other traffic in the radius).
  await expect(page.locator('[data-testid="track-row"][data-own="false"]').first()).toBeVisible();
  const prox = page.getByTestId("traffic-alerts").locator('[data-testid="alert-row"][data-kind="proximity"]').first();
  await expect(prox).toBeVisible();
  await expect(prox).toContainText("Converging aircraft");
  await expect(prox).toContainText("Authenticated");
  await shot(page, "traffic-en");

  // The alert stream: acknowledged, recorded with the time.
  await page.goto(`/alerts?intent=${intentId}`);
  const alert = page.getByTestId("stream-alerts").locator('[data-testid="alert-row"][data-kind="proximity"]').first();
  await expect(alert).toBeVisible();
  // api records the alert under its intent as soon as it is raised, even
  // before its flight is recorded, so the first acknowledgement answers:
  // one click, no retry.
  await alert.getByRole("button", { name: "Acknowledge" }).click();
  await expect(alert.getByTestId("acked")).toBeVisible();
  await expect(alert).toHaveAttribute("data-acked", "true");
  await shot(page, "alerts-en");

  // Georgian: the same pages in ka.
  await page.getByRole("button", { name: "ქართული" }).click();
  await expect(page.locator("html")).toHaveAttribute("lang", "ka");
  await expect(page.getByRole("heading", { level: 1 })).toHaveText("გაფრთხილებები");
  await shot(page, "alerts-ka");
  await page.goto(`/traffic?intent=${intentId}`);
  await expect(page.getByRole("heading", { level: 1 })).toHaveText("მოძრაობის ინფორმაცია");
  await expect(page.locator('[data-testid="track-row"][data-own="true"]')).toHaveCount(1);

  expect(offOrigin()).toEqual([]);
  await shot(page, "traffic-ka");
});

test("the pages in both languages, Noto Sans Georgian, the CSP, nothing off the origin", async ({ browser }) => {
  for (const [locale, lang, heading] of [
    ["ka-GE", "ka", "ოპერატორის პორტალში შესვლა"],
    ["en-GB", "en", "Sign in to the operator portal"],
  ] as const) {
    const ctx = await browser.newContext({ locale });
    const page = await ctx.newPage();
    const offOrigin = sameOrigin(page);
    const res = await page.goto("/login");
    expect(res?.headers()["content-security-policy"]).toContain("connect-src 'self'");
    expect(res?.headers()["content-security-policy"]).toContain("font-src 'self'");
    expect(res?.headers()["content-security-policy"]).toContain("worker-src blob:");
    await expect(page.locator("html")).toHaveAttribute("lang", lang);
    await expect(page.getByRole("heading", { level: 1 })).toHaveText(heading);
    // The heading's first family is the kit's Noto Sans Georgian (next/font
    // names it notoSansGeorgian), loaded from this origin.
    const family = await page.getByRole("heading", { level: 1 }).evaluate((el) => getComputedStyle(el).fontFamily);
    expect(family).toMatch(/^"?notoSansGeorgian/i);
    const loaded = await page.evaluate(async () => {
      await document.fonts.ready;
      return [...document.fonts].filter((f) => f.status === "loaded").map((f) => f.family);
    });
    if (lang === "ka") expect(loaded.some((f) => /notoSansGeorgian/i.test(f))).toBe(true);
    await page.screenshot({ path: test.info().outputPath(`login-${lang}.png`), fullPage: true });
    expect(offOrigin()).toEqual([]);
    await ctx.close();
  }
});

test("signed out, a portal page says how to sign in and the BFF refuses the API", async ({ page, request }) => {
  await page.goto("/intents");
  await expect(page.getByTestId("signed-out")).toBeVisible();
  const res = await request.get("/_bff/api/v1/intents");
  expect(res.status()).toBe(401);
  const outside = await request.get("/_bff/api/v1/admin/status");
  expect(outside.status()).toBe(404);
});

test("accessibility of the sign-in, intents form and traffic pages (axe, WCAG 2.2 AA)", async ({ page, request }) => {
  const u = unique();
  const number = `GEO-TEST-E2EA${u}`;
  await control(request, "/registry", { operator: number });
  const user = `a11y.${u}`;
  const pass = `portal-password-${u}`;
  const reg = await page.request.post("/_bff/api/v1/accounts/operators", {
    data: { registration_number: number, display_name: `A11y ${u}`, contact_email: `a${u}@example.test`, admin_username: user, admin_password: pass },
    headers: await csrfHeaders(page),
  });
  expect(reg.status(), await reg.text()).toBe(201);
  await page.goto("/login");
  const login = await new AxeBuilder(page).analyze();
  expect(login.violations).toEqual([]);
  expect(login.passes).toBeGreaterThan(10);
  await page.getByLabel("Username").fill(user);
  await page.getByLabel("Password").fill(pass);
  await page.getByRole("button", { name: "Sign in" }).click();
  await expect(page).toHaveURL(/\/intents$/);
  for (const path of ["/intents/new", "/traffic", "/geo", "/alerts", "/clients"]) {
    await page.goto(path);
    await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
    const r = await new AxeBuilder(page).analyze();
    expect(r.passes, path).toBeGreaterThan(10);
    expect(r.violations, `${path}: ${JSON.stringify(r.violations.map((v) => ({ id: v.id, nodes: v.nodes.map((n) => n.target) })))}`).toEqual([]);
  }
});

/** The CSRF cookie the proxy issues to a visitor, as the header the BFF checks. */
async function csrfHeaders(page: Page): Promise<Record<string, string>> {
  await page.goto("/register");
  const c = (await page.context().cookies()).find((x) => x.name === "uspace_csrf");
  return { "X-CSRF-Token": c?.value ?? "", Origin: "http://127.0.0.1:3200" };
}
