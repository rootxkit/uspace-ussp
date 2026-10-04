"use client";

// The page frame (brief WP-17): skip link, brand, the portal's
// navigation, the language switch (ka, en), the session and sign-out.
// What a role does not use is hidden (RequireRole); it grants nothing,
// the API decides every request.
import type { ReactNode } from "react";
import Link from "next/link";
import { usePathname, useRouter } from "next/navigation";
import { RequireRole, useSession } from "@rootxkit/uspace-ui/auth/client";
import { LANGS, useLang } from "@rootxkit/uspace-ui/i18n";
import { useTheme } from "@rootxkit/uspace-ui/theme";
import { Button } from "@rootxkit/uspace-ui/ui";
import { useRuntimeConfig } from "@/components/Providers";
import type { AppKey } from "@/i18n/catalogues";
import { useAppT } from "@/i18n/t";

const NAV: { href: string; key: AppKey; roles?: string[] }[] = [
  { href: "/intents", key: "portal.nav.intents" },
  { href: "/intents/new", key: "portal.nav.new_intent", roles: ["operator_admin", "remote_pilot"] },
  { href: "/geo", key: "portal.nav.geo" },
  { href: "/traffic", key: "portal.nav.traffic" },
  { href: "/alerts", key: "portal.nav.alerts" },
  { href: "/weather", key: "portal.nav.weather" },
  { href: "/clients", key: "portal.nav.clients" },
];

export function Shell({ children }: { children: ReactNode }) {
  const t = useAppT();
  const { lang, setLang } = useLang();
  const { session, signOut } = useSession();
  const { brand } = useTheme();
  const router = useRouter();
  const path = usePathname();
  const cfg = useRuntimeConfig();
  const link = (href: string, key: AppKey) => {
    const current = path === href;
    return (
      <Link
        key={href}
        href={href}
        aria-current={current ? "page" : undefined}
        className={current ? "font-semibold underline underline-offset-4" : "underline-offset-4 hover:underline"}
      >
        {t(key)}
      </Link>
    );
  };
  return (
    <>
      <a
        href="#main"
        className="sr-only focus:not-sr-only focus:absolute focus:left-2 focus:top-2 focus:z-50 focus:rounded focus:bg-[var(--us-surface-raised)] focus:p-2"
      >
        {t("portal.skip")}
      </a>
      <header className="flex flex-wrap items-center gap-x-6 gap-y-2 border-b border-[var(--us-border)] bg-[var(--us-surface-raised)] px-4 py-2">
        <Link href="/" className="font-bold">
          {brand.name}
        </Link>
        <nav aria-label={t("portal.nav.label")} className="flex flex-wrap gap-x-4 gap-y-1 text-sm">
          {session === null ? (
            <>
              {link("/login", "portal.nav.login")}
              {link("/register", "portal.nav.register")}
            </>
          ) : (
            NAV.map((n) =>
              n.roles === undefined ? (
                link(n.href, n.key)
              ) : (
                <RequireRole key={n.href} anyOf={n.roles}>
                  {link(n.href, n.key)}
                </RequireRole>
              ),
            )
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
                  router.push("/login");
                  router.refresh();
                });
              }}
            >
              {t("portal.nav.sign_out")}
            </Button>
          </div>
        )}
      </header>
      {session !== null && session.realm !== "portal" && (
        <p role="alert" className="m-0 border-b border-[var(--us-border)] px-4 py-2 text-sm" data-testid="realm-notice">
          {t("portal.session.wrong_realm", { realm: session.realm })}
        </p>
      )}
      <main id="main" tabIndex={-1} className="flex flex-col gap-4 p-4 focus:outline-none">
        {children}
      </main>
      <footer className="border-t border-[var(--us-border)] px-4 py-2 text-xs text-[var(--us-text-muted)]">
        {t("portal.footer.a11y", { target: cfg.accessibilityTarget, status: cfg.accessibilityStatus })}
      </footer>
    </>
  );
}
