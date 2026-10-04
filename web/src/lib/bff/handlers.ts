// The three BFF routes (/_bff/login, /_bff/logout, /_bff/api/*) on the
// kit's helpers, and nothing else (PLAN §3.2; M21, M22): no database, no
// bus, no key, no judgement, no ticket route. The BFF never verifies a
// token; the API decides every request. The traffic and alert
// WebSockets are opened by the browser on this origin and the session
// cookie rides the upgrade; they are never proxied here.
import { bffHandlers, type BffHandlers } from "@rootxkit/uspace-ui/auth/server";
import { NextResponse, type NextRequest } from "next/server";
import { bffEnv } from "../../config";
import { LOGIN_PATH, LOGOUT_PATH, PORTAL_REALM, allowPaths, withRealm } from "./paths";

type Handler = (req: NextRequest) => Promise<Response>;

let cached: BffHandlers | null = null;

function unavailable(detail: string): Handler {
  return () =>
    Promise.resolve(
      NextResponse.json(
        { type: "https://schemas.uspace.ge/problems/bff_unavailable", title: "Portal unavailable", status: 503, detail },
        { status: 503, headers: { "Content-Type": "application/problem+json", "Cache-Control": "no-store" } },
      ),
    );
}

/** The handlers of this process, built at the first request (the build has no environment). */
export function bff(): BffHandlers {
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
    apiLogoutPath: LOGOUT_PATH,
    session: { secure: cfg.secure, maxAgeS: cfg.sessionMaxAgeS },
    allowPaths: allowPaths(cfg.apiBase),
    timeoutMs: cfg.timeoutMs,
    ...(cfg.trustedProxyHops === null ? {} : { trustedProxyHops: cfg.trustedProxyHops }),
    // The kit's sign-in step sends {username, password}; this API's
    // login also takes the realm, which for this app is the portal's
    // (WP-18's console mounts its own handlers).
    fetch: (input, init) => fetch(input, withRealm(input, init, loginUrl, PORTAL_REALM)),
  });
  return cached;
}
