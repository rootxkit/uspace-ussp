"use client";

// The kit's providers in the order a web/ mounts them (uspace-ui
// docs/CONSUMING.md §3): the CSP nonce (Radix ScrollArea's injected
// style), the theme with the brand from the environment, the language
// negotiated on the server, the session's display claims (decoded
// unverified, display only), and the portal's runtime configuration.
import { createContext, useContext, type ReactNode } from "react";
import { useRouter } from "next/navigation";
import { SessionProvider } from "@rootxkit/uspace-ui/auth/client";
import { I18nProvider, LANG_COOKIE, type Lang } from "@rootxkit/uspace-ui/i18n";
import type { SessionDisplay } from "@rootxkit/uspace-ui/model";
import { ThemeProvider, type Brand } from "@rootxkit/uspace-ui/theme";
import { CspNonceProvider } from "@rootxkit/uspace-ui/ui";
import type { RuntimeConfig } from "@/config";
import { catalogues } from "@/i18n/catalogues";

const RuntimeContext = createContext<RuntimeConfig | null>(null);

/** The portal's runtime configuration (config/portal.json and the environment). */
export function useRuntimeConfig(): RuntimeConfig {
  const c = useContext(RuntimeContext);
  if (c === null) throw new Error("useRuntimeConfig outside Providers");
  return c;
}

export function Providers(props: {
  lang: Lang;
  brand: Brand;
  nonce: string | undefined;
  session: SessionDisplay | null;
  config: RuntimeConfig;
  children: ReactNode;
}) {
  const router = useRouter();
  return (
    <CspNonceProvider nonce={props.nonce}>
      <ThemeProvider brand={props.brand}>
        <I18nProvider
          lang={props.lang}
          catalogues={catalogues}
          onLangChange={(l) => {
            // The kit never writes the cookie; the app persists the choice
            // and asks the server to render in it.
            document.cookie = `${LANG_COOKIE}=${l}; Path=/; Max-Age=31536000; SameSite=Lax`;
            router.refresh();
          }}
        >
          <SessionProvider session={props.session}>
            <RuntimeContext.Provider value={props.config}>{props.children}</RuntimeContext.Provider>
          </SessionProvider>
        </I18nProvider>
      </ThemeProvider>
    </CspNonceProvider>
  );
}
