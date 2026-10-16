import { expect, test, type APIRequestContext } from "@playwright/test";
import { control, unique } from "./helpers";

// SC-17, the USSP clause (uspace-lab knowledge/scenarios.md), in the
// order staging ran it, end to end against the e2e stack
// (playwright.config.ts): the processes on the real databases and NATS
// with the fake authority, whose change feed api polls every 30 s.
//
// An operator and its UAS are registered at the authority and the
// operator files an intent before api's feed has seen the registration
// (the decision's lookup is fetched under a cursor behind it); the
// aircraft then flies inside TZE2E17, a PROHIBITED zone with AMSL
// limits. Once the feed has applied the registration the aircraft is
// still registered and no identification alert is raised over the
// flight, while the zone incursion is (the zone path judged it). The
// authority suspends the UAS mid-flight and the track is suspended
// within one poll interval. The presence twin: an aircraft whose serial
// the registry does not hold, flown without an intent in the same zone,
// raises the identification alert.

const API = "http://127.0.0.1:3301";
// TZE2E17 spans 41.90 to 41.91 N and 45.10 to 45.115 E (test/e2e/stack):
// a point near its west edge, so the intent filed 2 km or more east of
// it is outside the zone. Where in the zone and how far east vary per
// run, so a rerun on the same database does not meet the intent an
// earlier run left (first come, first served).
const IN_ZONE = { lat: 41.9015 + Math.random() * 0.007, lng: 45.102 };
const INTENT_EAST_M = 2000 + Math.floor(Math.random() * 4000);
// api's feed polls every 30 s (registry.DefaultFeedInterval).
const POLL_S = 30;

interface Alert {
  alert_id: string;
  kind: string;
  state: string;
  intent_id?: string;
  flight_id?: string;
}

async function supervisor(request: APIRequestContext): Promise<string> {
  const u = unique();
  const username = `supervisor.sc17.${u}`;
  const password = `staff-password-${u}`;
  await control(request, "/staff", { username, password, role: "supervisor" });
  const res = await request.post(`${API}/v1/accounts/login`, {
    data: { realm: "console", username, password },
  });
  expect(res.ok(), await res.text()).toBe(true);
  return String(((await res.json()) as { token: string }).token);
}

async function alerts(
  request: APIRequestContext,
  token: string,
): Promise<Alert[]> {
  const res = await request.get(`${API}/v1/admin/alerts?view=recent`, {
    headers: { Authorization: `Bearer ${token}` },
  });
  expect(res.ok(), await res.text()).toBe(true);
  return ((await res.json()) as { alerts: Alert[] }).alerts;
}

async function flightOf(
  request: APIRequestContext,
  token: string,
  serial: string,
): Promise<string> {
  const res = await request.get(`${API}/v1/admin/flights`, {
    headers: { Authorization: `Bearer ${token}` },
  });
  expect(res.ok(), await res.text()).toBe(true);
  const f = (
    (await res.json()) as {
      flights: { flight_id: string; uas_serial: string }[];
    }
  ).flights.find((x) => x.uas_serial === serial);
  return f?.flight_id ?? "";
}

/** The change feed's cursor as api's /readyz names it (registry entry). */
async function feedCursor(request: APIRequestContext): Promise<number> {
  const res = await request.get(`${API}/readyz`);
  const body = (await res.json()) as {
    dependencies: Record<string, { detail?: string }>;
  };
  const m = /feed_cursor=(\d+)/.exec(
    body.dependencies["registry"]?.detail ?? "",
  );
  return m ? Number(m[1]) : -1;
}

/** The identification of the intent's own track in its traffic product. */
async function ownIdent(
  request: APIRequestContext,
  intentId: string,
): Promise<string> {
  const p = (await control(request, "/traffic", { intent_id: intentId })) as {
    body?: {
      tracks?: { own?: boolean; identification?: { status?: string } }[];
    };
  };
  return p.body?.tracks?.find((t) => t.own)?.identification?.status ?? "none";
}

test.describe.configure({ mode: "serial" });

test("SC-17: a registration just before the flight stays registered in a prohibited zone; a suspension mid-flight is seen; an unregistered serial alerts", async ({
  request,
}) => {
  test.setTimeout(300_000);
  const token = await supervisor(request);

  // Registration, intent decision (the lookup) and the flight, all before
  // the feed's next poll names the registration.
  const a = await control(request, "/intruder", {
    lat: IN_ZONE.lat,
    lng: IN_ZONE.lng,
    east_m: 0,
    intent_east_m: INTENT_EAST_M,
  });
  const intentId = String(a["intent_id"]);
  const serial = String(a["serial"]);
  const registered = Number(a["registry_seq"]);
  expect(intentId).toMatch(/^[0-9a-f-]{36}$/);

  // The feed applies the page naming the registration.
  await expect
    .poll(() => feedCursor(request), {
      timeout: (2 * POLL_S + 15) * 1000,
      intervals: [1000],
    })
    .toBeGreaterThanOrEqual(registered);
  // The zone path judged the aircraft in the zone (presence) ...
  await expect
    .poll(
      async () =>
        (await alerts(request, token)).some(
          (x) => x.intent_id === intentId && x.kind === "zone_incursion",
        ),
      { timeout: 60_000 },
    )
    .toBe(true);
  // ... and over the next two poll intervals the aircraft stays registered
  // and no identification alert is ever raised for it.
  const until = Date.now() + 2 * POLL_S * 1000;
  while (Date.now() < until) {
    expect(await ownIdent(request, intentId)).toBe("registered");
    const ident = (await alerts(request, token)).filter(
      (x) => x.intent_id === intentId && x.kind === "identification",
    );
    expect(ident, JSON.stringify(ident)).toEqual([]);
    await new Promise((r) => setTimeout(r, 2000));
  }

  // The authority suspends the UAS: suspended within one poll interval
  // (and the time the projection takes to reach the hot path).
  const suspended = Number(
    (await control(request, "/registry/suspend", { serial }))["registry_seq"],
  );
  await expect
    .poll(() => feedCursor(request), {
      timeout: (POLL_S + 15) * 1000,
      intervals: [1000],
    })
    .toBeGreaterThanOrEqual(suspended);
  await expect
    .poll(() => ownIdent(request, intentId), { timeout: 15_000 })
    .toBe("suspended");
  expect(
    (await alerts(request, token)).filter(
      (x) => x.intent_id === intentId && x.kind === "identification",
    ),
  ).toEqual([]);

  // The presence twin: a serial the registry does not hold, in the same
  // zone, raises the identification alert.
  const b = await control(request, "/intruder", {
    lat: IN_ZONE.lat,
    lng: IN_ZONE.lng,
    east_m: 200,
    intent: false,
    register_serial: false,
  });
  const unregistered = String(b["serial"]);
  await expect
    .poll(() => flightOf(request, token, unregistered), { timeout: 30_000 })
    .toMatch(/^[0-9a-f-]{36}$/);
  const flightId = await flightOf(request, token, unregistered);
  await expect
    .poll(
      async () =>
        (await alerts(request, token)).some(
          (x) => x.flight_id === flightId && x.kind === "identification",
        ),
      { timeout: 60_000 },
    )
    .toBe(true);
});
