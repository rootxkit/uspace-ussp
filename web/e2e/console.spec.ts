import AxeBuilder from "./axe";
import { expect, test, type APIRequestContext, type Browser, type Page } from "@playwright/test";
import { control, sameOrigin, totp, unique } from "./helpers";

// The USSP console of brief WP-18 in the browser, end to end against the
// e2e stack (playwright.config.ts: the seven processes with every fake,
// and NATS through a proxy the test can cut): staff of each role sign in
// (an admin with the authenticator's code, the kit's second step); an
// unacknowledged proximity alert escalates and appears on the supervisor
// console, and the supervisor closes it with a reason; a source switched
// off shows who switched it off and when and its tracks show
// source_disabled until they age out, and a support viewer gets 403 on a
// switch; an emergency case is opened, noted and closed with every step
// in the audit; the policy is changed with its diff in the history; the
// console with each fake down (the CISP, the registry, the DSS, the
// ANSP) and with NATS down shows each input's state with its time; and
// every admin action left an events row with its actor and reason; an
// admin confirms the start of operations, ceases and restarts them, each
// delivered to the fake authority, while a supervisor reads and cannot
// act. In Georgian and in English, nothing off the origin, axe on every
// page.

const CONSOLE = "http://127.0.0.1:3200";

/** A full-page screenshot in the test's output (CI uploads it with the trace). */
async function shot(page: Page, name: string) {
  await page.screenshot({ path: test.info().outputPath(`${name}.png`), fullPage: true });
}

interface Staff {
  username: string;
  password: string;
  id: string;
  totpSecret: string;
}

async function staff(request: APIRequestContext, role: "supervisor" | "support" | "admin"): Promise<Staff> {
  const u = unique();
  const username = `${role}.e2e.${u}`;
  const password = `staff-password-${u}`;
  const out = await control(request, "/staff", { username, password, role });
  return { username, password, id: String(out["id"]), totpSecret: String(out["totp_secret"] ?? "") };
}

/** The last TOTP step each admin used: a code is accepted once (the API keeps the step), so the next sign-in waits for a fresh one. */
const usedStep = new Map<string, number>();

async function signIn(page: Page, who: Staff, admin = false) {
  await page.goto("/console/login");
  await page.getByLabel("Username").fill(who.username);
  await page.getByLabel("Password").fill(who.password);
  await page.getByRole("button", { name: "Sign in" }).click();
  if (admin) {
    const last = usedStep.get(who.username) ?? -1;
    await expect.poll(() => Math.floor(Date.now() / 30_000), { timeout: 35_000 }).toBeGreaterThan(last);
    await page.getByLabel("One-time code").fill(totp(who.totpSecret));
    usedStep.set(who.username, Math.floor(Date.now() / 30_000));
    await page.getByRole("button", { name: "Sign in" }).click();
  }
  await expect(page).toHaveURL(/\/console$/);
  await expect(page.getByTestId("inputs-strip")).toBeVisible();
}

async function english(browser: Browser): Promise<Page> {
  const ctx = await browser.newContext({ locale: "en-GB", baseURL: CONSOLE });
  return ctx.newPage();
}

/** The CSRF cookie of the page's context, as the console BFF checks it. */
async function csrf(page: Page): Promise<Record<string, string>> {
  const c = (await page.context().cookies()).find((x) => x.name === "uspace_csrf");
  return { "X-CSRF-Token": c?.value ?? "", Origin: CONSOLE };
}

const M_PER_DEG_LAT = 111_320;

/**
 * Two aircraft of two operators 30 m apart at (lat, lng), each flying
 * an intent of its own 2 km east of where it was asked (so the intents
 * do not meet strategically): a proximity alert nobody acknowledges.
 */
async function converging(request: APIRequestContext, lat: number, lng: number) {
  const a = await control(request, "/intruder", { lat, lng, east_m: 0 });
  const westM = 3000;
  const bLng = lng - westM / (M_PER_DEG_LAT * Math.cos((lat * Math.PI) / 180));
  const b = await control(request, "/intruder", { lat, lng: bLng, east_m: westM + 30 });
  return { a, b };
}

test.describe.configure({ mode: "serial" });

test("S-M5: an unacknowledged alert escalates and appears; the supervisor closes it with a reason and the audit shows it", async ({ browser, request }) => {
  const page = await english(browser);
  const offOrigin = sameOrigin(page);
  const sup = await staff(request, "supervisor");
  const lat = 41.62 + Math.random() * 0.15;
  const lng = 44.65 + Math.random() * 0.3;
  const { a, b } = await converging(request, lat, lng);
  const ours = `[data-intent-id="${String(a["intent_id"])}"], [data-intent-id="${String(b["intent_id"])}"]`;
  await signIn(page, sup);
  // The live map by viewport: the two aircraft, live.
  await expect(page.getByTestId("console-feed")).toHaveAttribute("data-connection", "live");
  await expect(page.locator('[data-testid="console-track-row"][data-state="live"]').first()).toBeVisible({ timeout: 30_000 });
  // The CPA thresholds in force, as the status frame carries them (audit M-5).
  const thresholds = page.locator('[data-panel="thresholds"]');
  await expect(thresholds).toHaveAttribute("data-thresholds", "present");
  await expect(thresholds.locator('[data-threshold="cpa_vertical_min_m"]')).toContainText(/\d m$/);
  await expect(thresholds.locator('[data-threshold="cpa_clear_after_s"]')).toContainText(/\d s$/);
  // escalation_after_s (30 s) after the raise, the escalation appears.
  // This run's alert (the database keeps every earlier run's too).
  const esc = page.getByTestId("escalations-table").locator('[data-testid="admin-alert-row"][data-kind="proximity"][data-escalated="true"]').and(page.locator(ours)).first();
  await expect(esc).toBeVisible({ timeout: 90_000 });
  await expect(esc.getByTestId("admin-escalated")).toContainText("automatically");
  await shot(page, "console-map-escalation-en");
  const alertId = (await esc.getAttribute("data-alert-id")) ?? "";
  expect(alertId).toMatch(/^[0-9a-f-]{36}$/);

  await page.goto("/console/alerts");
  const row = page.getByTestId("console-alerts").locator(`[data-testid="admin-alert-row"][data-alert-id="${alertId}"]`);
  await expect(row).toBeVisible();
  await expect(row.getByTestId("messages-recorded")).toContainText("messages recorded");
  await row.getByLabel("Reason").fill("operator reached by phone; both aircraft separated");
  await row.getByRole("button", { name: "Close" }).click();
  await expect(row).toHaveAttribute("data-closed", "true");
  // Closing never clears the alert: the monitor alone does.
  await expect(row).not.toHaveAttribute("data-state", "cleared");
  await expect(page.getByTestId("escalations-table").locator(`[data-alert-id="${alertId}"]`)).toHaveCount(0);
  await row.getByRole("button", { name: "Audit" }).click();
  const audit = row.getByTestId("audit").locator('[data-testid="audit-row"][data-event="alert_closed"]');
  await expect(audit).toHaveAttribute("data-actor", sup.id);
  await expect(audit).toContainText("operator reached by phone");
  await shot(page, "console-alerts-closed-en");

  // The audit walk of the alert: every console write has its actor and reason.
  const ev = await page.request.get(`/_bff/console/api/v1/admin/events?entity_type=alert&entity_id=${alertId}`);
  expect(ev.status()).toBe(200);
  const rows = ((await ev.json()) as { events: { actor_id: string; event_type: string; payload: { reason?: string } }[] }).events;
  expect(rows.map((r) => r.event_type)).toContain("alert_closed");
  for (const r of rows) {
    expect(r.actor_id, JSON.stringify(r)).toBe(sup.id);
    expect(r.payload.reason, JSON.stringify(r)).toBeTruthy();
  }

  // Georgian.
  await page.getByRole("button", { name: "ქართული" }).click();
  await expect(page.locator("html")).toHaveAttribute("lang", "ka");
  await expect(page.getByRole("heading", { level: 1 })).toHaveText("გაფრთხილებები");
  await shot(page, "console-alerts-ka");
  expect(String(a["client_id"])).toMatch(/^op-/);
  expect(offOrigin()).toEqual([]);
  await page.context().close();
});

// The pair of the thresholds shown above: a status frame without
// thresholds{} and evaluation_period_s (traffic-ws's frames, rewritten in
// the browser's WebSocket route) shows none and says so; nothing is
// defaulted (INV-03).
test("a status frame without thresholds shows none and says so", async ({ browser, request }) => {
  const page = await english(browser);
  await page.routeWebSocket(/\/v1\/traffic(\?|$)/, (ws) => {
    const server = ws.connectToServer();
    server.onMessage((message) => {
      if (typeof message !== "string") return ws.send(message);
      const frame = JSON.parse(message) as { schema?: string; body?: Record<string, unknown> };
      if (frame.schema === "console/status/v1" && frame.body !== undefined) {
        delete frame.body["thresholds"];
        delete frame.body["evaluation_period_s"];
      }
      ws.send(JSON.stringify(frame));
    });
  });
  await signIn(page, await staff(request, "supervisor"));
  await expect(page.getByTestId("console-feed")).toHaveAttribute("data-connection", "live");
  const thresholds = page.locator('[data-panel="thresholds"]');
  await expect(thresholds).toHaveAttribute("data-thresholds", "absent");
  await expect(thresholds.locator("[data-thresholds-none]")).toHaveText("The system sends no thresholds in its status; none is assumed.");
  await expect(thresholds.locator("[data-threshold]")).toHaveCount(0);
});

test("a source switched off shows who and when, its tracks show source_disabled and age out; a support viewer gets 403", async ({ browser, request }) => {
  const adminPage = await english(browser);
  const ad = await staff(request, "admin");
  const viewer = await staff(request, "support");
  const lat = 41.62 + Math.random() * 0.15;
  const lng = 44.65 + Math.random() * 0.3;
  const flying = await control(request, "/intruder", { lat, lng, east_m: 0 });
  const client = String(flying["client_id"]);
  const input = `operator_ws/${client}`;
  await signIn(adminPage, ad, true);
  await expect(adminPage.locator('[data-testid="console-track-row"][data-state="live"]').first()).toBeVisible({ timeout: 30_000 });

  // A support viewer reads the inputs and may not switch: 403, nothing written.
  const viewerPage = await english(browser);
  await signIn(viewerPage, viewer);
  await viewerPage.goto("/console/inputs");
  const vrow = viewerPage.locator(`[data-testid="input-row"][data-input="${input}"]`);
  await expect(vrow).toHaveAttribute("data-state", "healthy", { timeout: 30_000 });
  await expect(vrow).toContainText("Read only");
  const refused = await viewerPage.request.post("/_bff/console/api/v1/admin/sources", {
    data: { source_type: "operator_ws", instance_id: client, enabled: false, reason: "viewer tries" },
    headers: await csrf(viewerPage),
  });
  expect(refused.status()).toBe(403);
  await shot(viewerPage, "console-inputs-viewer-en");

  // The admin switches the client off with a reason.
  await adminPage.goto("/console/inputs");
  const row = adminPage.locator(`[data-testid="input-row"][data-input="${input}"]`);
  await expect(row).toHaveAttribute("data-state", "healthy", { timeout: 30_000 });
  await row.getByLabel("Reason").fill("client sends out-of-order samples");
  await row.getByRole("button", { name: "Switch off" }).click();
  await expect(row).toHaveAttribute("data-state", "disabled");
  await expect(row.getByTestId("disabled-by")).toContainText(`switched off by ${ad.username}`);
  await expect(row.getByTestId("disabled-by")).toContainText("client sends out-of-order samples");
  await expect(adminPage.locator(`[data-testid="strip-input"][data-input="${input}"]`)).toHaveAttribute("data-state", "disabled");
  await expect(adminPage.locator(`[data-testid="switch-row"][data-input="${input}"]`)).toContainText(ad.username);
  await shot(adminPage, "console-inputs-disabled-en");
  // Its track shows source_disabled, then ages out of the product.
  await adminPage.goto("/console");
  const tracks = adminPage.locator('[data-testid="console-track-row"]');
  await expect(adminPage.locator('[data-testid="console-track-row"][data-state="source_disabled"]').first()).toBeVisible({ timeout: 30_000 });
  await shot(adminPage, "console-map-source-disabled-en");
  await expect(adminPage.locator('[data-testid="console-track-row"][data-state="source_disabled"]')).toHaveCount(0, { timeout: 120_000 });
  expect(await tracks.count()).toBeGreaterThanOrEqual(0);

  // Reversible: on again, with its own audit row.
  await adminPage.goto("/console/inputs");
  await row.getByLabel("Reason").fill("client fixed");
  await row.getByRole("button", { name: "Switch on" }).click();
  await expect(row).not.toHaveAttribute("data-state", "disabled");
  const ev = await adminPage.request.get(`/_bff/console/api/v1/admin/events?entity_type=source_control&entity_id=${encodeURIComponent(input)}`);
  const rows = ((await ev.json()) as { events: { actor_id: string; payload: { reason?: string; enabled?: boolean } }[] }).events;
  expect(rows.map((r) => r.payload.enabled)).toEqual([false, true]);
  for (const r of rows) {
    expect(r.actor_id).toBe(ad.id);
    expect(r.payload.reason).toBeTruthy();
  }
  await adminPage.context().close();
  await viewerPage.context().close();
});

test("the emergency workflow: a case opened, noted and closed, every step in the audit", async ({ browser, request }) => {
  const page = await english(browser);
  const sup = await staff(request, "supervisor");
  const lat = 41.62 + Math.random() * 0.15;
  const lng = 44.65 + Math.random() * 0.3;
  const flying = await control(request, "/intruder", { lat, lng, east_m: 0 });
  await signIn(page, sup);
  await page.goto("/console/flights");
  const flight = page.locator(`[data-testid="flight-row"][data-serial="${String(flying["serial"])}"]`);
  await expect(flight).toBeVisible({ timeout: 30_000 });
  await flight.getByTestId("flight-emergency-link").click();
  await expect(page).toHaveURL(/\/console\/emergency\/[0-9a-f-]{36}$/);
  await page.getByLabel("Reason").fill("the remote pilot reports a lost link and a fly-away");
  await page.getByRole("button", { name: "Open the case" }).click();
  const c = page.getByTestId("case");
  await expect(c).toHaveAttribute("data-open", "true");
  await expect(c.getByTestId("contact-ref")).toContainText("EC-TEST-B");
  await page.getByLabel("Note", { exact: true }).fill("called the operator's number of record; the pilot confirms");
  await page.getByLabel("Checklist step it completes").selectOption("operator_contacted");
  await page.getByRole("button", { name: "Add the note" }).click();
  await expect(c.locator('[data-testid="checklist-step"][data-step="operator_contacted"]')).toHaveAttribute("data-done", "true");
  await expect(c.getByTestId("note")).toHaveCount(1);
  await page.getByLabel("Outcome").fill("the aircraft landed in the contingency area");
  await page.getByRole("button", { name: "Close the case" }).click();
  await expect(c).toHaveAttribute("data-open", "false");
  await expect(c.getByTestId("case-closed")).toContainText("landed");
  const audit = c.getByTestId("audit");
  for (const e of ["emergency_case_opened", "emergency_case_note", "emergency_case_closed"]) {
    await expect(audit.locator(`[data-testid="audit-row"][data-event="${e}"]`)).toHaveAttribute("data-actor", sup.id);
  }
  await shot(page, "console-emergency-closed-en");
  const caseId = (await c.getAttribute("data-case-id")) ?? "";
  const ev = await page.request.get(`/_bff/console/api/v1/admin/events?entity_type=emergency_case&entity_id=${caseId}`);
  const rows = ((await ev.json()) as { events: { actor_id: string; payload: Record<string, unknown> }[] }).events;
  expect(rows).toHaveLength(3);
  for (const r of rows) {
    expect(r.actor_id).toBe(sup.id);
    expect(r.payload["reason"] ?? r.payload["text"] ?? r.payload["outcome"]).toBeTruthy();
  }
  await page.getByRole("button", { name: "ქართული" }).click();
  await expect(page.getByRole("heading", { level: 1 })).toHaveText("საგანგებო შემთხვევა");
  await shot(page, "console-emergency-ka");
  await page.context().close();
});

test("the policy: an admin's change is a new version with its diff; every role reads it", async ({ browser, request }) => {
  const page = await english(browser);
  const ad = await staff(request, "admin");
  await signIn(page, ad, true);
  await page.goto("/console/policy");
  const editor = page.getByTestId("policy-editor");
  await expect(editor).toBeVisible();
  const before = Number(await page.getByTestId("policy-current").getAttribute("data-version"));
  const field = page.getByLabel("cis_stale_s", { exact: true });
  const old = await field.inputValue();
  const next = String(Number(old) + 1);
  await field.fill(next);
  await expect(page.getByTestId("policy-preview-changes")).toContainText(`cis_stale_s: ${old} → ${next}`);
  await page.getByLabel("Reason", { exact: true }).fill("WP-18 e2e: the CIS staleness bound tried one second longer");
  await page.getByRole("button", { name: "Store as a new version" }).click();
  await expect(page.getByTestId("policy-stored")).toBeVisible();
  await expect(page.getByTestId("policy-current")).not.toHaveAttribute("data-version", String(before));
  const head = page.getByTestId("policy-version").first();
  await expect(head.getByTestId("policy-changes")).toContainText(`cis_stale_s: ${old} → ${next}`);
  await expect(head).toContainText(ad.username);
  await expect(page.getByText("pending GCAA").first()).toBeVisible();
  await shot(page, "console-policy-en");
  const v = (await page.getByTestId("policy-current").getAttribute("data-version")) ?? "";
  const ev = await page.request.get(`/_bff/console/api/v1/admin/events?entity_type=policy&entity_id=${v}`);
  const rows = ((await ev.json()) as { events: { actor_id: string; payload: { reason?: string } }[] }).events;
  expect(rows).toHaveLength(1);
  expect(rows[0]?.actor_id).toBe(ad.id);
  expect(rows[0]?.payload.reason).toContain("WP-18 e2e");
  // Restore the bound for the next runs on this database.
  await field.fill(old);
  await page.getByLabel("Reason", { exact: true }).fill("WP-18 e2e: restored");
  await page.getByRole("button", { name: "Store as a new version" }).click();
  await expect(page.getByTestId("policy-current")).not.toHaveAttribute("data-version", v);
  await page.context().close();
});

/** The inputs page's row of api's dependency dep. */
function dependencyRow(page: Page, dep: string) {
  return page.locator(`[data-testid="dependency"][data-dependency="${dep}"]`);
}

test("each fake down, then NATS down: every input with its state and its time", async ({ browser, request }) => {
  test.setTimeout(900_000);
  const page = await english(browser);
  const sup = await staff(request, "supervisor");
  await signIn(page, sup);
  await page.goto("/console/inputs");
  // Every fake up: the dependencies of api as they are, each with its time.
  await expect(dependencyRow(page, "cis")).toBeVisible();
  await expect(dependencyRow(page, "dss")).toBeVisible();
  await shot(page, "console-inputs-all-up-en");
  const down = async (fake: string, check: () => Promise<void>) => {
    await control(request, "/fake", { name: fake, down: true });
    try {
      await check();
      await shot(page, `console-${fake}-down-en`);
    } finally {
      await control(request, "/fake", { name: fake, down: false });
    }
  };
  // The CISP down: the CIS cache keeps its last version and shows its
  // age (it is degraded once older than cis_stale_s, 300 s by default).
  await down("cisp", async () => {
    await page.goto("/console/inputs");
    await expect(dependencyRow(page, "cis")).toContainText("age");
  });
  // The registry down: its change feed's poll fails (every 30 s), said with the time.
  await down("registry", async () => {
    await page.goto("/console/inputs");
    await expect(dependencyRow(page, "registry")).toContainText("last poll failed", { timeout: 120_000 });
    await expect(dependencyRow(page, "registry")).toHaveAttribute("data-state", "degraded");
  });
  // The ANSP down and its stream cut: the monitor's ansp_feed input is unavailable since T.
  await down("ansp", async () => {
    await page.goto("/console/inputs");
    const feed = page.locator('[data-testid="input-row"][data-input="ansp_feed"]');
    await expect.poll(async () => ["unreachable", "stale"].includes((await feed.getAttribute("data-state")) ?? ""), { timeout: 120_000 }).toBe(true);
    await expect(feed).toContainText("since");
  });
  // The DSS down: the DSS panel says down since T (the availability poll, every 60 s).
  await down("dss", async () => {
    await page.goto("/console/dss");
    await expect(page.getByTestId("dss-readiness")).toHaveAttribute("data-state", "down", { timeout: 120_000 });
    await expect(page.getByTestId("dss-detail")).toContainText("down since");
  });
  // NATS down: the console loads, the bus is not connected and every
  // input that is not switched off is unknown since then.
  await control(request, "/fake", { name: "nats", down: true });
  try {
    await page.goto("/console/inputs");
    await expect(page.getByTestId("strip-bus")).toHaveAttribute("data-state", "disconnected", { timeout: 60_000 });
    const strip = page.locator('[data-testid="strip-input"]');
    await expect.poll(async () => {
      const states = await strip.evaluateAll((els) => els.map((e) => e.getAttribute("data-state")));
      return states.length > 0 && states.every((s) => s === "unknown" || s === "disabled");
    }, { timeout: 30_000 }).toBe(true);
    await expect(strip.first()).toContainText("since");
    await expect(page.getByTestId("strip-monitor")).not.toHaveAttribute("data-state", "up");
    await shot(page, "console-nats-down-en");
    // traffic-ws closes its streams with 1013 while its bus is down
    // (05 §6: the console freezes with age shown), so the feed is not live.
    await page.goto("/console");
    await expect(page.getByTestId("console-feed")).not.toHaveAttribute("data-connection", "live", { timeout: 60_000 });
    await shot(page, "console-map-nats-down-en");
  } finally {
    await control(request, "/fake", { name: "nats", down: false });
  }
  await page.goto("/console/inputs");
  await expect(page.getByTestId("strip-bus")).toHaveAttribute("data-state", "connected", { timeout: 60_000 });
  await expect(page.getByTestId("strip-monitor")).toHaveAttribute("data-state", "up", { timeout: 60_000 });
  // The bus back: the same feed is live again (the presence pair).
  await page.goto("/console");
  await expect(page.getByTestId("console-feed")).toHaveAttribute("data-connection", "live", { timeout: 60_000 });
  await page.context().close();
});

test("the overview counts every operator's intents by state; a new intent raises the total by one", async ({ browser, request }) => {
  const page = await english(browser);
  const viewer = await staff(request, "support");
  await signIn(page, viewer);
  const counts = page.getByTestId("console-intent-counts");
  await expect(counts).toHaveAttribute("data-truncated", "false");
  await expect(page.getByTestId("console-intent-count-activated")).toBeVisible();
  await expect(page.getByRole("group", { name: "The service now" })).toContainText("Intents, every operator");
  await expect(page.getByRole("group", { name: "The service now" })).toContainText("Intents nonconforming or contingent");
  const before = Number(await counts.getAttribute("data-total"));
  expect(Number.isInteger(before)).toBe(true);
  // Another operator files and activates an intent: the next read counts it.
  await control(request, "/intruder", { lat: 41.62 + Math.random() * 0.15, lng: 44.65 + Math.random() * 0.3, east_m: 0 });
  await expect(counts).toHaveAttribute("data-total", String(before + 1), { timeout: 30_000 });
  await shot(page, "console-overview-counts-en");
  await page.context().close();
});

test("operations status: an admin confirms the start and it is delivered, ceases and restarts; a supervisor reads and cannot act", async ({ browser, request }) => {
  test.setTimeout(300_000);
  const STATUS_API = "/_bff/console/api/v1/admin/status";
  // This run's certificate has no notice yet (test/e2e/stack names one of its own).
  const supPage = await english(browser);
  const sup = await staff(request, "supervisor");
  await signIn(supPage, sup);
  await supPage.getByRole("link", { name: "Operations status" }).click();
  await expect(supPage).toHaveURL(/\/console\/status$/);
  await expect(supPage.getByRole("heading", { level: 1 })).toHaveText("Operations status");
  await expect(supPage.getByTestId("status-state")).toHaveAttribute("data-state", "not_started");
  await expect(supPage.getByTestId("status-certificate").locator("dd")).toHaveText(/^[0-9a-f]{32}$/);
  await expect(supPage.getByTestId("status-no-notices")).toBeVisible();
  // The supervisor reads: no action is offered, and the API refuses one sent anyway (403, nothing stored).
  await expect(supPage.getByTestId("status-read-only")).toBeVisible();
  await expect(supPage.getByTestId("status-action")).toHaveCount(0);
  const refused = await supPage.request.post(STATUS_API, { data: { kind: "start" }, headers: await csrf(supPage) });
  expect(refused.status()).toBe(403);
  // What the deployment's /readyz said: degraded until a start is recorded.
  await supPage.goto("/console/inputs");
  await expect(dependencyRow(supPage, "operating_status")).toHaveAttribute("data-state", "degraded", { timeout: 30_000 });
  await expect(dependencyRow(supPage, "operating_status")).toContainText("no start notice yet");
  const r0 = await new AxeBuilder(supPage).analyze();
  expect(r0.violations).toEqual([]);
  await supPage.goto("/console/status");
  await expect(supPage.getByTestId("status-no-notices")).toBeVisible();
  const r1 = await new AxeBuilder(supPage).analyze();
  expect(r1.passes).toBeGreaterThan(10);
  expect(r1.violations, JSON.stringify(r1.violations)).toEqual([]);

  // The admin is offered the start only, behind a dialog that says what it does.
  const page = await english(browser);
  const ad = await staff(request, "admin");
  await signIn(page, ad, true);
  await page.goto("/console/status");
  await expect(page.getByTestId("status-state")).toHaveAttribute("data-state", "not_started");
  const certificate = ((await page.getByTestId("status-certificate").locator("dd").textContent()) ?? "").trim();
  expect(certificate).toMatch(/^[0-9a-f]{32}$/);
  await expect(page.getByTestId("status-action")).toHaveCount(1);
  await page.getByRole("button", { name: "Confirm start of operations" }).click();
  const dialog = page.getByRole("alertdialog");
  await expect(dialog).toContainText("notifies the civil aviation authority");
  await expect(dialog).toContainText(`changes the status of certificate ${certificate}`);
  const rd = await new AxeBuilder(page).analyze();
  expect(rd.violations, JSON.stringify(rd.violations)).toEqual([]);
  // Cancel sends nothing.
  await dialog.getByRole("button", { name: "Cancel" }).click();
  await expect(dialog).toHaveCount(0);
  await expect(page.getByTestId("status-no-notices")).toBeVisible();
  await page.getByRole("button", { name: "Confirm start of operations" }).click();
  await dialog.getByRole("button", { name: "Confirm start of operations" }).click();
  await expect(page.getByTestId("status-answer")).toHaveAttribute("data-created", "true");
  await expect(page.getByTestId("status-state")).toHaveAttribute("data-state", "operating");
  const start = page.locator('[data-testid="status-notice"][data-kind="start"]');
  await expect(start).toHaveCount(1);
  await expect(start).toContainText(ad.id);
  // Sent after the commit: the fake authority records it and the row says delivered with its notice id.
  await expect(start).toHaveAttribute("data-state", "delivered", { timeout: 60_000 });
  await expect(start).toContainText("Authority's notice id");
  await shot(page, "console-status-operating-en");
  await page.goto("/console/inputs");
  await expect(dependencyRow(page, "operating_status")).toHaveAttribute("data-state", "up", { timeout: 60_000 });

  // Operating: the cease is offered, then the restart, each delivered in turn.
  await page.goto("/console/status");
  await expect(page.getByRole("button", { name: "Confirm start of operations" })).toHaveCount(0);
  await page.getByRole("button", { name: "Cease operations" }).click();
  await expect(dialog).toContainText(`at the authority to ceased`);
  await dialog.getByRole("button", { name: "Cease operations" }).click();
  await expect(page.getByTestId("status-state")).toHaveAttribute("data-state", "ceased");
  await expect(page.locator('[data-testid="status-notice"][data-kind="cease"]')).toHaveAttribute("data-state", "delivered", { timeout: 60_000 });
  await page.getByRole("button", { name: "Restart", exact: true }).click();
  await expect(dialog).toContainText(`at the authority to restarted`);
  await dialog.getByRole("button", { name: "Restart", exact: true }).click();
  await expect(page.getByTestId("status-state")).toHaveAttribute("data-state", "operating");
  await expect(page.locator('[data-testid="status-notice"][data-kind="restart"]')).toHaveAttribute("data-state", "delivered", { timeout: 60_000 });
  await expect(page.getByTestId("status-notice")).toHaveCount(3);
  await shot(page, "console-status-restarted-en");

  // A second start answers the stored one (200): nothing new is stored.
  const again = await page.request.post(STATUS_API, { data: { kind: "start" }, headers: await csrf(page) });
  expect(again.status()).toBe(200);
  // The admin's own POST without the CSRF pair is refused at the BFF.
  const noCsrf = await page.request.post(STATUS_API, { data: { kind: "cease" }, headers: { Origin: CONSOLE } });
  expect(noCsrf.status()).toBe(403);
  expect(((await noCsrf.json()) as { type: string }).type).toContain("csrf_refused");
  // Only GET and POST pass the BFF on the exact path; its neighbours are not let through.
  for (const method of ["PUT", "PATCH", "DELETE", "HEAD"]) {
    const res = await page.request.fetch(STATUS_API, { method, headers: await csrf(page) });
    expect(res.status(), method).toBe(405);
    expect(res.headers()["allow"], method).toBe("GET, POST");
  }
  for (const path of ["/v1/admin/status/1", "/v1/admin/statuses", "/v1/admin/statu", "/v1/admin/status/start"]) {
    const res = await page.request.get(`/_bff/console/api${path}`);
    expect(res.status(), path).toBe(404);
  }
  expect((await page.request.post("/_bff/console/api/v1/admin/statuses", { data: { kind: "cease" }, headers: await csrf(page) })).status()).toBe(404);
  await page.goto("/console/status");
  await expect(page.getByTestId("status-notice")).toHaveCount(3);

  // The supervisor sees the state the admin left, still without an action.
  await supPage.reload();
  await expect(supPage.getByTestId("status-state")).toHaveAttribute("data-state", "operating");
  await expect(supPage.getByTestId("status-action")).toHaveCount(0);
  await shot(supPage, "console-status-supervisor-en");
  await page.context().close();
  await supPage.context().close();
});

test("signed out, a console page says how to sign in; the console BFF refuses what is not the console's", async ({ page, request }) => {
  await page.goto("/console/alerts");
  await expect(page.getByTestId("console-signed-out")).toBeVisible();
  expect((await request.get("/_bff/console/api/v1/admin/alerts")).status()).toBe(401);
  expect((await request.get("/_bff/console/api/v1/intents")).status()).toBe(404);
  expect((await request.get("/_bff/api/v1/admin/inputs")).status()).toBe(404);
  // The console's intent summary is read only at its BFF: a GET reaches
  // the API (401 signed out), any other method is 405 before the proxy;
  // the portal's BFF does not let it through.
  expect((await request.get("/_bff/console/api/v1/admin/intents/summary")).status()).toBe(401);
  const post = await request.post("/_bff/console/api/v1/admin/intents/summary", { data: {} });
  expect(post.status()).toBe(405);
  expect(post.headers()["allow"]).toBe("GET, HEAD");
  expect((await request.get("/_bff/api/v1/admin/intents/summary")).status()).toBe(404);
  expect((await request.get("/_bff/console/api/v1/intents/summary")).status()).toBe(404);
});

test("accessibility of the console pages (axe, WCAG 2.2 AA)", async ({ browser, request }) => {
  const page = await english(browser);
  await page.goto("/console/login");
  const login = await new AxeBuilder(page).analyze();
  expect(login.violations).toEqual([]);
  const ad = await staff(request, "admin");
  await signIn(page, ad, true);
  for (const path of ["/console", "/console/flights", "/console/alerts", "/console/emergency", "/console/dss", "/console/inputs", "/console/policy", "/console/status", "/console/occurrences", "/console/records"]) {
    await page.goto(path);
    await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
    await expect(page.getByTestId("inputs-strip")).not.toContainText("Loading");
    const r = await new AxeBuilder(page).analyze();
    expect(r.passes, path).toBeGreaterThan(10);
    expect(r.violations, `${path}: ${JSON.stringify(r.violations.map((v) => ({ id: v.id, nodes: v.nodes.map((n) => n.target) })))}`).toEqual([]);
  }
  // The dark scheme, as the top bar's toggle keeps it: the same pages at the same level.
  await page.getByTestId("scheme-toggle").click();
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
  for (const path of ["/console", "/console/alerts", "/console/inputs", "/console/policy", "/console/status"]) {
    await page.goto(path);
    await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
    await expect(page.getByRole("heading", { level: 1 })).toBeVisible();
    await expect(page.getByTestId("inputs-strip")).not.toContainText("Loading");
    const r = await new AxeBuilder(page).analyze();
    expect(r.passes, `${path} dark`).toBeGreaterThan(10);
    expect(r.violations, `${path} dark: ${JSON.stringify(r.violations.map((v) => ({ id: v.id, nodes: v.nodes.map((n) => n.target) })))}`).toEqual([]);
  }
  await page.context().close();
});
