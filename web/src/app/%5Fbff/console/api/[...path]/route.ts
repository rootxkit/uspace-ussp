// /_bff/console/api/*: the session cookie as a bearer to the console's
// allowed API paths, the CSRF double submit checked on every unsafe
// method (M21; brief WP-18).
import { consoleProxy } from "@/lib/bff/console";

export const GET = consoleProxy;
export const POST = consoleProxy;
export const PUT = consoleProxy;
export const PATCH = consoleProxy;
export const DELETE = consoleProxy;
