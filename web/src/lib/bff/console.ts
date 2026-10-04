// The console's BFF handlers (brief WP-18): the kit's three routes again,
// mounted at /_bff/console/{login,logout,api/*} beside the portal's, with
// the console's realm and allow-list, and the kit's two-step sign-in of
// a staff admin (the password step answers a challenge, sealed in the
// HttpOnly uspace_mfa cookie under USSP_WEB_BFF_SECRET; the second step
// sends it with the code to the API's /v1/accounts/login/mfa). The BFF
// never verifies a token: the API decides every request (realm console
// and the role of each operation).
import { BFF_API_PREFIX, bffHandlers, type BffHandlers } from "@rootxkit/uspace-ui/auth/server";
import { NextRequest, NextResponse } from "next/server";
import { bffEnv } from "../../config";
import { CONSOLE_BFF, CONSOLE_REALM, LOGIN_PATH, LOGOUT_PATH, MFA_PATH, consoleAllowPaths, withRealm } from "./paths";

const CONSOLE_API = CONSOLE_BFF.api;

type Handler = (req: NextRequest) => Promise<Response>;

let cached: BffHandlers | null = null;

function unavailable(detail: string): Handler {
  return () =>
    Promise.resolve(
      NextResponse.json(
        { type: "https://schemas.uspace.ge/problems/bff_unavailable", title: "Console unavailable", status: 503, detail },
        { status: 503, headers: { "Content-Type": "application/problem+json", "Cache-Control": "no-store" } },
      ),
    );
}

/** The console's handlers of this process, built at the first request. */
export function consoleBff(): BffHandlers {
  if (cached !== null) return cached;
  const env = bffEnv();
  if ("problem" in env) {
    const off = unavailable(env.problem);
    return { login: off, logout: off, proxy: off };
  }
  const { cfg } = env;
  const loginUrl = new URL(new URL(cfg.apiBase).pathname.replace(/\/$/, "") + LOGIN_PATH, cfg.apiBase).href;
  cached = bffHandlers({
    apiBase: cfg.apiBase,
    apiLoginPath: LOGIN_PATH,
    ...(cfg.mfaSecret === null ? {} : { apiMfaPath: MFA_PATH, mfaChallengeSecret: cfg.mfaSecret }),
    apiLogoutPath: LOGOUT_PATH,
    session: { secure: cfg.secure, maxAgeS: cfg.sessionMaxAgeS },
    allowPaths: consoleAllowPaths(cfg.apiBase),
    timeoutMs: cfg.timeoutMs,
    ...(cfg.trustedProxyHops === null ? {} : { trustedProxyHops: cfg.trustedProxyHops }),
    // The kit's password step sends {username, password}; this API's
    // login also takes the realm: the console's.
    fetch: (input, init) => fetch(input, withRealm(input, init, loginUrl, CONSOLE_REALM)),
  });
  return cached;
}

/**
 * The console's proxy: the kit's proxy serves only paths under its fixed
 * /_bff/api prefix (KIT: pending uspace-ui, a prefix per handler set), so
 * /_bff/console/api/<path> is handed to it as /_bff/api/<path>; the
 * console's allow-list, not the portal's, decides what passes.
 */
export function consoleProxy(req: NextRequest): Promise<Response> {
  const url = new URL(req.url);
  if (!url.pathname.startsWith(`${CONSOLE_API}/`)) {
    return Promise.resolve(NextResponse.json({ type: "https://schemas.uspace.ge/problems/not_found", title: "Not found", status: 404 }, { status: 404 }));
  }
  url.pathname = BFF_API_PREFIX + url.pathname.slice(CONSOLE_API.length);
  const hasBody = req.method !== "GET" && req.method !== "HEAD";
  // A streamed body needs duplex "half" (Node's fetch); the DOM's
  // RequestInit does not name it.
  const init = { method: req.method, headers: req.headers, ...(hasBody ? { body: req.body, duplex: "half" } : {}) } as NonNullable<ConstructorParameters<typeof NextRequest>[1]>;
  return consoleBff().proxy(new NextRequest(url, init));
}
