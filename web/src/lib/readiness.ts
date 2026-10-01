import "server-only";
import type { components } from "@/api/types";

type Readiness = components["schemas"]["Readiness"];

export type ReadinessResult =
  | { reachable: true; httpStatus: number; body: Readiness }
  | { reachable: false; error: string };

// The BFF target: the api process, reached from the server only. The
// browser never talks to it directly and never sees this URL.
const apiBase = process.env.USSP_WEB_API_URL ?? "http://127.0.0.1:8080";

// readReadiness reads /readyz of the API. 200 and 503 both carry the
// readiness body; anything else, and no answer within 3 s, is reported
// as unreachable with the reason, never as an empty list.
export async function readReadiness(): Promise<ReadinessResult> {
  try {
    const res = await fetch(new URL("/readyz", apiBase), { cache: "no-store", signal: AbortSignal.timeout(3000) });
    if (res.status !== 200 && res.status !== 503) {
      return { reachable: false, error: `HTTP ${res.status}` };
    }
    return { reachable: true, httpStatus: res.status, body: (await res.json()) as Readiness };
  } catch (err) {
    const cause = err instanceof Error && err.cause instanceof Error ? `: ${err.cause.message}` : "";
    return { reachable: false, error: (err instanceof Error ? err.message : String(err)) + cause };
  }
}
