// /_bff/console/logout: ends the console session at the API and clears the cookies.
import { type NextRequest } from "next/server";
import { consoleBff } from "@/lib/bff/console";

export function POST(req: NextRequest): Promise<Response> {
  return consoleBff().logout(req);
}
