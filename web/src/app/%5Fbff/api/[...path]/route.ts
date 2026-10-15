// /_bff/api/*: the session cookie as a bearer to the allowed API paths,
// the CSRF double submit checked on every unsafe method (M21); a
// GET-only path refuses every other method before the proxy.
import { type NextRequest } from "next/server";
import { BFF_API_PREFIX } from "@rootxkit/uspace-ui/auth/server";
import { bff, refuseMethod } from "@/lib/bff/handlers";
import { getOnlyPaths } from "@/lib/bff/paths";

function proxy(req: NextRequest): Promise<Response> {
  const refused = refuseMethod(new URL(req.url).pathname.slice(BFF_API_PREFIX.length), req.method, getOnlyPaths());
  if (refused !== null) return Promise.resolve(refused);
  return bff().proxy(req);
}

export const GET = proxy;
export const POST = proxy;
export const PUT = proxy;
export const PATCH = proxy;
export const DELETE = proxy;
