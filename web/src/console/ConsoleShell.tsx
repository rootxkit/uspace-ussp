"use client";

// The USSP console's frame (brief WP-18): the kit's AppShell with the
// console's navigation in groups, the feed pill of the overview's live
// feed, the scheme toggle, the language switch (ka, en), the user menu
// with sign-out, and the persistent inputs strip on every page. A page is
// shown for a console session (realm console, from the session's claims);
// a portal session or none gets the sign-in frame with the way to the
// console's sign-in. The navigation grants nothing: the API decides every
// request. Nothing here reaches an aircraft.
import type { ReactNode } from "react";
import Link from "next/link";
import { usePathname } from "next/navigation";
import { useSession } from "@rootxkit/uspace-ui/auth/client";
import { useTheme } from "@rootxkit/uspace-ui/theme";
import { AppShell, type Crumb } from "@rootxkit/uspace-ui/ui";
import { useRuntimeConfig } from "@/components/Providers";
import { AuthFrame, CrumbContext, FeedPill, FeedPillProvider, SchemeToggle, SessionMenu, ShellContent, renderLink, useFeedPillState } from "@/components/frame";
import {
  AlertIcon,
  ArchiveIcon,
  DashboardIcon,
  EmergencyIcon,
  FlightIcon,
  Mark,
  NetworkIcon,
  PolicyIcon,
  ReportIcon,
  SourceIcon,
} from "@/components/icons";
import { currentEntry, navGroups, type NavSection } from "@/components/nav";
import { useAppT } from "@/i18n/t";
import { InputsProvider, InputsStrip } from "./InputsStrip";

const NAV: NavSection[] = [
  {
    id: "operations",
    key: "console.nav.group.operations",
    items: [
      { href: "/console", key: "console.nav.overview", icon: <DashboardIcon /> },
      { href: "/console/flights", key: "console.nav.flights", icon: <FlightIcon /> },
      { href: "/console/alerts", key: "console.nav.alerts", icon: <AlertIcon /> },
      { href: "/console/emergency", key: "console.nav.emergency", icon: <EmergencyIcon />, prefix: true },
    ],
  },
  {
    id: "systems",
    key: "console.nav.group.systems",
    items: [
      { href: "/console/inputs", key: "console.nav.inputs", icon: <SourceIcon /> },
      { href: "/console/dss", key: "console.nav.dss", icon: <NetworkIcon /> },
    ],
  },
  {
    id: "governance",
    key: "console.nav.group.governance",
    items: [
      { href: "/console/policy", key: "console.nav.policy", icon: <PolicyIcon /> },
      { href: "/console/occurrences", key: "console.nav.occurrences", icon: <ReportIcon /> },
      { href: "/console/records", key: "console.nav.records", icon: <ArchiveIcon /> },
    ],
  },
];

export const LOGIN = "/console/login";

export function ConsoleShell({ children }: { children: ReactNode }) {
  const t = useAppT();
  const { session } = useSession();
  const { brand } = useTheme();
  const path = usePathname();
  const cfg = useRuntimeConfig();
  const [feed, setFeed] = useFeedPillState();
  const staff = session !== null && session.realm === "console";
  const onLogin = path === LOGIN;
  const name = t("console.brand", { brand: brand.name });
  if (!staff || onLogin) {
    return (
      <AuthFrame home="/console" name={name} footer={t("console.footer")}>
        {onLogin ? (
          children
        ) : (
          <p role="status" data-testid="console-signed-out" className="m-0 rounded-panel border border-border bg-card p-4 text-sm shadow-panel">
            {session === null ? t("console.signed_out") : t("console.wrong_realm", { realm: session.realm })}{" "}
            <Link href={LOGIN} className="font-semibold underline">
              {t("portal.nav.login")}
            </Link>
          </p>
        )}
      </AuthFrame>
    );
  }
  const current = currentEntry(NAV, path);
  const group = NAV.find((g) => current !== undefined && g.items.includes(current));
  const crumbs: Crumb[] = [{ label: t("console.nav.overview"), href: "/console" }, ...(group?.key !== undefined ? [{ label: t(group.key) }] : [])];
  return (
    <AppShell
      systemName={name}
      logo={<Mark />}
      nav={navGroups(NAV, current, session.roles, t)}
      renderLink={renderLink}
      status={<FeedPill value={feed} />}
      actions={<SchemeToggle />}
      user={<SessionMenu signedOutPath={LOGIN} />}
    >
      <InputsProvider>
        <ShellContent footer={`${t("console.footer")} ${t("portal.footer.a11y", { target: cfg.accessibilityTarget, status: cfg.accessibilityStatus })}`}>
          <InputsStrip />
          <FeedPillProvider set={setFeed}>
            <CrumbContext.Provider value={path === "/console" ? null : crumbs}>{children}</CrumbContext.Provider>
          </FeedPillProvider>
        </ShellContent>
      </InputsProvider>
    </AppShell>
  );
}
