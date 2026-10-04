"use client";

// The API from the console's pages (brief WP-18): always through
// /_bff/console/api, which forwards the session cookie as a bearer to the
// console's allowed paths; unsafe requests carry X-CSRF-Token (M21). The
// kit's client never retries; a non-2xx answer rejects with an ApiError
// carrying the problem (M28). A 401 sends the person to the console's
// sign-in.
import { useMemo } from "react";
import { useRouter } from "next/navigation";
import { createClient } from "@rootxkit/uspace-ui/api";
import { csrfToken } from "@rootxkit/uspace-ui/auth/client";
import { useLang } from "@rootxkit/uspace-ui/i18n";
import type { paths } from "@/api/generated/openapi";
import type { Api } from "./api";
import { CONSOLE_BFF } from "./bff/paths";

export function useConsoleApi(): Api {
  const { lang } = useLang();
  const router = useRouter();
  return useMemo(
    () =>
      createClient<paths>({
        baseUrl: CONSOLE_BFF.api,
        csrfToken: () => csrfToken(),
        lang: () => lang,
        onUnauthorized: () => router.push("/console/login"),
      }),
    [lang, router],
  );
}
