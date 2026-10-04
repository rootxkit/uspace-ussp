// What the BFF may reach and how it signs in (brief WP-17). Pure
// functions, unit-testable without Next.js.

/** The API's sign-in and sign-out (api/openapi.yaml, accounts). */
export const LOGIN_PATH = "/v1/accounts/login";
export const LOGOUT_PATH = "/v1/accounts/logout";
/** The session realm of every portal sign-in (M20). */
export const PORTAL_REALM = "portal";

const ID = "[0-9a-fA-F-]{36}";
const CLIENT = "[A-Za-z0-9._-]{1,128}";

/**
 * The upstream paths the portal's pages call, matched against the whole
 * upstream path (which starts with apiBase's own path). Anything else is
 * a 404 at the BFF, whatever the API would answer.
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
    `/v1/intents/${ID}`,
    "/v1/geo",
    `/v1/geo/intents/${ID}`,
    "/v1/weather",
    `/v1/alerts/${ID}/ack`,
    "/v1/traffic/snapshot",
  ].map((p) => new RegExp(`^${prefix}${p}$`));
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
