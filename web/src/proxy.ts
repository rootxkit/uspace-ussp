// Next.js request proxy (the middleware), for pages only:
//
// - a fresh CSP nonce per page, set (never read) on a copy of the
//   request headers, so a client-sent x-nonce is overwritten; the layout
//   hands it to CspNonceProvider and Next.js stamps its scripts with it;
// - the uspace_csrf cookie for a visitor without one (M21: the BFF
//   checks the double submit on every unsafe request, and the one unsafe
//   request before a session is the self-registration). A session's
//   sign-in replaces it.
import { CSP_NONCE_HEADER, CSRF_COOKIE, issueCspNonce, issueCsrf } from "@rootxkit/uspace-ui/auth/server";
import { NextResponse, type NextRequest } from "next/server";
import { bffEnv } from "./config";
import { contentSecurityPolicy } from "./csp";

export function proxy(req: NextRequest): NextResponse {
  const nonce = issueCspNonce();
  const csp = contentSecurityPolicy(nonce, process.env.NODE_ENV === "development");
  const headers = new Headers(req.headers);
  headers.set(CSP_NONCE_HEADER, nonce);
  headers.set("Content-Security-Policy", csp);
  const res = NextResponse.next({ request: { headers } });
  res.headers.set("Content-Security-Policy", csp);
  if (req.cookies.get(CSRF_COOKIE) === undefined) {
    const env = bffEnv();
    if ("cfg" in env) issueCsrf(res, { secure: env.cfg.secure, maxAgeS: env.cfg.sessionMaxAgeS });
  }
  return res;
}

export const config = {
  // Pages only: not Next's assets, the BFF, the basemap or the favicon.
  matcher: ["/((?!_next/|_bff/|basemap/|favicon\\.ico).*)"],
};
