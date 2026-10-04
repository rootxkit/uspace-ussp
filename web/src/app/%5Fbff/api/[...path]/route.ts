// /_bff/api/*: the session cookie as a bearer to the allowed API paths,
// the CSRF double submit checked on every unsafe method (M21).
import { type NextRequest } from "next/server";
import { bff } from "@/lib/bff/handlers";

function proxy(req: NextRequest): Promise<Response> {
  return bff().proxy(req);
}

export const GET = proxy;
export const POST = proxy;
export const PUT = proxy;
export const PATCH = proxy;
export const DELETE = proxy;
