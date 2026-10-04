"use client";

// The API from the browser: always through /_bff/api, which forwards the
// session cookie as a bearer; unsafe requests carry X-CSRF-Token from the
// uspace_csrf cookie (M21). The kit's client never retries and rejects a
// non-2xx answer with an ApiError carrying the problem (M28). A 401 sends
// the person to sign in again.
import { useMemo } from "react";
import { useRouter } from "next/navigation";
import { createClient, type Client } from "@rootxkit/uspace-ui/api";
import { csrfToken } from "@rootxkit/uspace-ui/auth/client";
import { useLang } from "@rootxkit/uspace-ui/i18n";
import type { components, paths } from "@/api/generated/openapi";

export type Api = Client<paths>;
export type Schemas = components["schemas"];

export const BFF_API = "/_bff/api";

export function useApi(): Api {
  const { lang } = useLang();
  const router = useRouter();
  return useMemo(
    () =>
      createClient<paths>({
        baseUrl: BFF_API,
        csrfToken: () => csrfToken(),
        lang: () => lang,
        onUnauthorized: () => router.push("/login"),
      }),
    [lang, router],
  );
}
