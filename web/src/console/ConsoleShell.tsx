"use client";

// The USSP console's frame (brief WP-18): skip link, brand, the console's
// navigation, the language switch (ka, en), the session and sign-out,
// and the persistent inputs strip on every page. A page is shown for a
// console session (realm console, from the session's claims); a portal
// session or none gets the way to the console's sign-in. What a role does
// not use is hidden (RequireRole) and grants nothing: the API decides
// every request. Nothing here reaches an aircraft.
import type { ReactNode } from "react";
import Link from "next/link";
import { usePathname, useRouter } from "next/navigation";
import { useSession } from "@rootxkit/uspace-ui/auth/client";
import { LANGS, useLang } from "@rootxkit/uspace-ui/i18n";
import { useTheme } from "@rootxkit/uspace-ui/theme";
import { Button } from "@rootxkit/uspace-ui/ui";
import { useRuntimeConfig } from "@/components/Providers";
import type { AppKey } from "@/i18n/catalogues";
import { useAppT } from "@/i18n/t";
import { InputsProvider, InputsStrip } from "./InputsStrip";

const NAV: { href: string; key: AppKey }[] = [
  { href: "/console", key: "console.nav.map" },
  { href: "/console/flights", key: "console.nav.flights" },
  { href: "/console/alerts", key: "console.nav.alerts" },
  { href: "/console/emergency", key: "console.nav.emergency" },
  { href: "/console/dss", key: "console.nav.dss" },
  { href: "/console/inputs", key: "console.nav.inputs" },
  { href: "/console/policy", key: "console.nav.policy" },
  { href: "/console/occurrences", key: "console.nav.occurrences" },
  { href: "/console/records", key: "console.nav.records" },
];

export const LOGIN = "/console/login";

export function ConsoleShell({ children }: { children: ReactNode }) {
  const t = useAppT();
  const { lang, setLang } = useLang();
  const { session, signOut } = useSession();
  const { brand } = useTheme();
  const router = useRouter();
  const path = usePathname();
  const cfg = useRuntimeConfig();
  const staff = session !== null && session.realm === "console";
  const onLogin = path === LOGIN;
  return (
    <>
      <a
        href="#main"
        className="sr-only focus:not-sr-only focus:absolute focus:left-2 focus:top-2 focus:z-50 focus:rounded focus:bg-[var(--us-surface-raised)] focus:p-2"
      >
        {t("portal.skip")}
      </a>
      <header className="flex flex-wrap items-center gap-x-6 gap-y-2 border-b border-[var(--us-border)] bg-[var(--us-surface-raised)] px-4 py-2">
        <Link href="/console" className="font-bold">
          {t("console.brand", { brand: brand.name })}
        </Link>
        <nav aria-label={t("console.nav.label")} className="flex flex-wrap gap-x-4 gap-y-1 text-sm">
          {staff
            ? NAV.map((n) => {
                const current = path === n.href;
                return (
                  <Link
                    key={n.href}
                    href={n.href}
                    aria-current={current ? "page" : undefined}
                    className={current ? "font-semibold underline underline-offset-4" : "underline-offset-4 hover:underline"}
                  >
                    {t(n.key)}
                  </Link>
                );
              })
            : !onLogin && (
                <Link href={LOGIN} className="underline-offset-4 hover:underline">
                  {t("portal.nav.login")}
                </Link>
              )}
        </nav>
        <div role="group" aria-label={t("portal.lang.label")} className="ml-auto flex gap-2">
          {LANGS.map((l) => (
            <Button key={l} size="sm" variant={l === lang ? "default" : "outline"} lang={l} aria-pressed={l === lang} onClick={() => setLang(l)}>
              {t(l === "ka" ? "portal.lang.ka" : "portal.lang.en")}
            </Button>
          ))}
        </div>
        {session !== null && (
          <div className="flex items-center gap-2 text-sm" data-testid="session">
            <span>{t("portal.session.roles", { roles: session.roles.join(", ") })}</span>
            <Button
              size="sm"
              variant="outline"
              onClick={() => {
                void signOut().then(() => {
                  router.push(LOGIN);
                  router.refresh();
                });
              }}
            >
              {t("portal.nav.sign_out")}
            </Button>
          </div>
        )}
      </header>
      {staff && !onLogin ? (
        <InputsProvider>
          <InputsStrip />
          <main id="main" tabIndex={-1} className="flex flex-col gap-4 p-4 focus:outline-none">
            {children}
          </main>
        </InputsProvider>
      ) : (
        <main id="main" tabIndex={-1} className="flex flex-col gap-4 p-4 focus:outline-none">
          {onLogin ? (
            children
          ) : (
            <p role="status" data-testid="console-signed-out">
              {session === null ? t("console.signed_out") : t("console.wrong_realm", { realm: session.realm })}{" "}
              <Link href={LOGIN} className="underline">
                {t("portal.nav.login")}
              </Link>
            </p>
          )}
        </main>
      )}
      <footer className="border-t border-[var(--us-border)] px-4 py-2 text-xs text-[var(--us-text-muted)]">
        {t("console.footer")} {t("portal.footer.a11y", { target: cfg.accessibilityTarget, status: cfg.accessibilityStatus })}
      </footer>
    </>
  );
}
