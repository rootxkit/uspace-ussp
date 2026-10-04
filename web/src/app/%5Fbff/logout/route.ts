// /_bff/logout: ends the session at the API and clears both cookies.
import { type NextRequest } from "next/server";
import { bff } from "@/lib/bff/handlers";

export function POST(req: NextRequest): Promise<Response> {
  return bff().logout(req);
}
