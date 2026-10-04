// /_bff/login: the kit's sign-in on the API's /v1/accounts/login (realm portal).
import { type NextRequest } from "next/server";
import { bff } from "@/lib/bff/handlers";

export function POST(req: NextRequest): Promise<Response> {
  return bff().login(req);
}
