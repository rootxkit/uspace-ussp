"use client";

// The page frame (brief WP-17): the kit's AppShell with the portal's
// navigation in groups, the feed pill of a page with a live feed, the
// scheme toggle, the language switch (ka, en) and the user menu with
// sign-out. Signed out, and on the sign-in and sign-up pages, the
// sign-in frame instead. What a role does not use is hidden; it grants
// nothing, the API decides every request.
import type { ReactNode } from "react";
import { usePathname } from "next/navigation";
import { useSession } from "@rootxkit/uspace-ui/auth/client";
import { useTheme } from "@rootxkit/uspace-ui/theme";
import { AppShell, type Crumb } from "@rootxkit/uspace-ui/ui";
import { useRuntimeConfig } from "@/components/Providers";
import { ConsoleShell } from "@/console/ConsoleShell";
import { useAppT } from "@/i18n/t";
import { AuthFrame, CrumbContext, FeedPill, FeedPillProvider, SchemeToggle, SessionMenu, ShellContent, renderLink, useFeedPillState } from "./frame";
import { currentEntry, navGroups, type NavSection } from "./nav";
import { AircraftIcon, AlertIcon, DashboardIcon, IntentIcon, KeyIcon, Mark, PlusIcon, WeatherIcon, ZoneIcon } from "./icons";

const NAV: NavSection[] = [
  { id: "overview", items: [{ href: "/", key: "portal.nav.overview", icon: <DashboardIcon /> }] },
  {
    id: "flights",
    key: "portal.nav.group.flights",
    items: [
      { href: "/intents", key: "portal.nav.intents", icon: <IntentIcon />, prefix: true },
      { href: "/intents/new", key: "portal.nav.new_intent", icon: <PlusIcon />, roles: ["operator_admin", "remote_pilot"] },
      { href: "/traffic", key: "portal.nav.traffic", icon: <AircraftIcon /> },
      { href: "/alerts", key: "portal.nav.alerts", icon: <AlertIcon /> },
    ],
  },
  {
    id: "planning",
    key: "portal.nav.group.planning",
    items: [
      { href: "/geo", key: "portal.nav.geo", icon: <ZoneIcon /> },
      { href: "/weather", key: "portal.nav.weather", icon: <WeatherIcon /> },
    ],
  },
  { id: "account", key: "portal.nav.group.account", items: [{ href: "/clients", key: "portal.nav.clients", icon: <KeyIcon /> }] },
];

/** The sign-in and sign-up pages, framed without navigation. */
const AUTH_PATHS = ["/login", "/register"];

/** The frame of the page: the console's under /console (brief WP-18), the portal's elsewhere. */
export function Shell({ children }: { children: ReactNode }) {
  const path = usePathname();
  if (path === "/console" || path.startsWith("/console/")) return <ConsoleShell>{children}</ConsoleShell>;
  return <PortalShell>{children}</PortalShell>;
}

function PortalShell({ children }: { children: ReactNode }) {
  const t = useAppT();
  const { session } = useSession();
  const { brand } = useTheme();
  const path = usePathname();
  const cfg = useRuntimeConfig();
  const [feed, setFeed] = useFeedPillState();
  if (session === null || AUTH_PATHS.includes(path)) {
    return (
      <AuthFrame home="/" name={brand.name} wide={path !== "/login"}>
        {children}
      </AuthFrame>
    );
  }
  const current = currentEntry(NAV, path);
  const group = NAV.find((g) => current !== undefined && g.items.includes(current));
  const crumbs: Crumb[] = [{ label: t("portal.nav.overview"), href: "/" }, ...(group?.key !== undefined ? [{ label: t(group.key) }] : [])];
  return (
    <AppShell
      systemName={brand.name}
      logo={<Mark />}
      nav={navGroups(NAV, current, session.roles, t)}
      renderLink={renderLink}
      status={<FeedPill value={feed} />}
      actions={<SchemeToggle />}
      user={<SessionMenu signedOutPath="/login" />}
    >
      <ShellContent footer={t("portal.footer.a11y", { target: cfg.accessibilityTarget, status: cfg.accessibilityStatus })}>
        {session.realm !== "portal" && (
          <p role="alert" className="m-0 rounded-panel border border-status-warning-border bg-status-warning-soft px-4 py-3 text-sm" data-testid="realm-notice">
            {t("portal.session.wrong_realm", { realm: session.realm })}
          </p>
        )}
        <FeedPillProvider set={setFeed}>
          <CrumbContext.Provider value={path === "/" ? null : crumbs}>{children}</CrumbContext.Provider>
        </FeedPillProvider>
      </ShellContent>
    </AppShell>
  );
}
