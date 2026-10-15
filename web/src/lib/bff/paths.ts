// What the BFF may reach and how it signs in (brief WP-17). Pure
// functions, unit-testable without Next.js.

/** The API's sign-in and sign-out (api/openapi.yaml, accounts). */
export const LOGIN_PATH = "/v1/accounts/login";
export const LOGOUT_PATH = "/v1/accounts/logout";
/** The session realm of every portal sign-in (M20). */
export const PORTAL_REALM = "portal";
/** The session realm of every console sign-in (M20; brief WP-18). */
export const CONSOLE_REALM = "console";
/** The API's second sign-in step of a staff admin (WP-18). */
export const MFA_PATH = "/v1/accounts/login/mfa";
/** The console's BFF routes (its own handlers beside the portal's, under the kit's /_bff, where the sealed MFA cookie lives). */
export const CONSOLE_BFF = { login: "/_bff/console/login", logout: "/_bff/console/logout", api: "/_bff/console/api" } as const;

const ID = "[0-9a-fA-F-]{36}";
const CLIENT = "[A-Za-z0-9._-]{1,128}";

/**
 * The upstream paths the portal's pages call, matched against the whole
 * upstream path (which starts with apiBase's own path). Anything else is
 * a 404 at the BFF, whatever the API would answer. Only api's routes:
 * traffic-ws's (the streams, the snapshot) are not behind the BFF, and
 * internal/national's TestBFFAllowListIsAPIRoutes holds this list to
 * api's access table.
 */
export function allowPaths(apiBase: string): RegExp[] {
  const prefix = new URL(apiBase).pathname.replace(/\/$/, "").replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  return [
    "/v1/accounts/me",
    "/v1/accounts/operators",
    `/v1/accounts/operators/${ID}`,
    `/v1/accounts/operators/${ID}/clients`,
    `/v1/accounts/operators/${ID}/clients/${CLIENT}/rotate`,
    `/v1/accounts/operators/${ID}/clients/${CLIENT}/serials`,
    `/v1/accounts/operators/${ID}/clients/${CLIENT}/serials/[A-Za-z0-9._%-]{1,192}`,
    "/v1/intents",
    "/v1/intents/summary",
    `/v1/intents/${ID}`,
    "/v1/geo",
    `/v1/geo/intents/${ID}`,
    "/v1/weather",
    `/v1/alerts/${ID}/ack`,
  ].map((p) => new RegExp(`^${prefix}${p}$`));
}

/**
 * The upstream paths the console's pages call (brief WP-18), matched as
 * allowPaths' are: api's console operations and nothing of the
 * portal's. internal/national's TestBFFAllowListIsAPIRoutes holds this
 * list to api's access table too.
 */
export function consoleAllowPaths(apiBase: string): RegExp[] {
  const prefix = new URL(apiBase).pathname.replace(/\/$/, "").replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
  return [
    "/v1/accounts/me",
    "/v1/admin/flights",
    "/v1/admin/intents/summary",
    "/v1/admin/alerts",
    "/v1/admin/escalations",
    `/v1/admin/alerts/${ID}/escalate`,
    `/v1/admin/alerts/${ID}/close`,
    "/v1/admin/dss",
    "/v1/admin/inputs",
    "/v1/admin/policy",
    "/v1/admin/sources",
    "/v1/admin/emergency",
    `/v1/admin/emergency/${ID}`,
    "/v1/admin/occurrences",
    "/v1/admin/coordination",
    "/v1/admin/status",
    "/v1/admin/records/days",
    "/v1/admin/events",
  ].map((p) => new RegExp(`^${prefix}${p}$`));
}

/**
 * The allowed paths of the portal's BFF that pass a GET (or HEAD) and
 * nothing else: the read-only summaries. Matched against the path after
 * the BFF's own prefix, before the kit's proxy sees the request; every
 * path here is in allowPaths too (internal/national's
 * TestBFFGetOnlyPathsAreGetRoutes holds both to api's access table).
 */
export function getOnlyPaths(): RegExp[] {
  return ["/v1/intents/summary"].map((p) => new RegExp(`^${p}$`));
}

/** The console's GET-only paths, as getOnlyPaths' are, all in consoleAllowPaths. */
export function consoleGetOnlyPaths(): RegExp[] {
  return ["/v1/admin/intents/summary"].map((p) => new RegExp(`^${p}$`));
}

/** An allowed path that takes exactly these methods (upper case) and no other. */
export interface MethodEntry {
  path: RegExp;
  methods: readonly string[];
}

/**
 * The console's paths that take exactly the methods named, matched as
 * consoleGetOnlyPaths' are: the operating-status notices (Art. 7(6)),
 * read with GET (listStatusNotices) and asked for with POST
 * (requestStatusNotice, an admin's at the API), and no HEAD, PUT, PATCH
 * or DELETE. Every path here is in consoleAllowPaths, and api serves it
 * with exactly these methods (internal/national's
 * TestConsoleBFFMethodPathsAreAPIRoutes).
 */
export function consoleMethodPaths(): MethodEntry[] {
  const entries: [string, string[]][] = [["/v1/admin/status", ["GET", "POST"]]];
  return entries.map(([p, methods]) => ({ path: new RegExp(`^${p}$`), methods }));
}

/**
 * The methods path (the path after the BFF's prefix) takes, or null
 * when it is left to the allow-list and the API: GET and HEAD on a
 * GET-only path, the entry's methods on a method path. A path that does
 * not decode is judged as it came.
 */
export function allowedMethods(path: string, getOnly: readonly RegExp[], methodPaths: readonly MethodEntry[] = []): readonly string[] | null {
  let decoded = path;
  try {
    decoded = decodeURIComponent(path);
  } catch {
    // judged as it came
  }
  const hit = (re: RegExp) => re.test(path) || re.test(decoded);
  if (getOnly.some(hit)) return ["GET", "HEAD"];
  return methodPaths.find((e) => hit(e.path))?.methods ?? null;
}

/**
 * Whether method may go to path (the path after the BFF's prefix): a
 * GET-only path takes GET and HEAD only, a method path its own methods;
 * any other path is left to the allow-list and the API.
 */
export function methodAllowed(path: string, method: string, getOnly: readonly RegExp[], methodPaths: readonly MethodEntry[] = []): boolean {
  const allowed = allowedMethods(path, getOnly, methodPaths);
  return allowed === null || allowed.includes(method);
}

/**
 * The fetch init of the API's sign-in with the realm added to its JSON
 * body; any other request passes unchanged. A body that is not a JSON
 * object passes unchanged too, and the API refuses it.
 */
export function withRealm(
  input: RequestInfo | URL,
  init: RequestInit | undefined,
  loginUrl: string,
  realm: string,
): RequestInit | undefined {
  const url = typeof input === "string" ? input : input instanceof URL ? input.href : input.url;
  if (url !== loginUrl || init?.method !== "POST" || typeof init.body !== "string") return init;
  let body: unknown;
  try {
    body = JSON.parse(init.body);
  } catch {
    return init;
  }
  if (typeof body !== "object" || body === null || Array.isArray(body)) return init;
  return { ...init, body: JSON.stringify({ ...(body as Record<string, unknown>), realm }) };
}
