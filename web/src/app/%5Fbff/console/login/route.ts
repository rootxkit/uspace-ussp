// /_bff/console/login: the kit's sign-in on the API's /v1/accounts/login
// (realm console), and a staff admin's second step on
// /v1/accounts/login/mfa (brief WP-18).
import { type NextRequest } from "next/server";
import { consoleBff } from "@/lib/bff/console";

export function POST(req: NextRequest): Promise<Response> {
  return consoleBff().login(req);
}
