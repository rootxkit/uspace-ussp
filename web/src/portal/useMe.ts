"use client";

// The signed-in account as the API knows it (GET /v1/accounts/me): its
// operator and roles. The session cookie's claims are display only; this
// is what the pages that need the operator id read.
import { useEffect, useState } from "react";
import { useSession } from "@rootxkit/uspace-ui/auth/client";
import { useApi, type Schemas } from "@/lib/api";

export type MeState =
  | { kind: "signed_out" }
  | { kind: "loading" }
  | { kind: "loaded"; me: Schemas["Me"] }
  | { kind: "failed"; error: unknown };

type Answer = { sub: string; state: MeState };

export function useMe(): MeState {
  const api = useApi();
  const { session } = useSession();
  const sub = session?.sub ?? null;
  const [answer, setAnswer] = useState<Answer | null>(null);
  useEffect(() => {
    if (sub === null) return;
    let live = true;
    api
      .GET("/v1/accounts/me")
      .then(({ data }) => live && data !== undefined && setAnswer({ sub, state: { kind: "loaded", me: data } }))
      .catch((error: unknown) => live && setAnswer({ sub, state: { kind: "failed", error } }));
    return () => {
      live = false;
    };
  }, [api, sub]);
  if (sub === null) return { kind: "signed_out" };
  return answer !== null && answer.sub === sub ? answer.state : { kind: "loading" };
}

/** Whether the roles may file, change and acknowledge (the API decides; this hides what a viewer cannot use). */
export function writes(roles: readonly string[]): boolean {
  return roles.includes("operator_admin") || roles.includes("remote_pilot");
}
