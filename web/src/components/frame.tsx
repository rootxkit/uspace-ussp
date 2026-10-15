"use client";

// The pieces both consoles' frames share (the operator portal's and the
// USSP console's, each on the kit's AppShell): the page's header with its
// trail, the frame of the sign-in pages, the scheme toggle, the user
// menu, and the top bar's feed pill, which a page with a live feed lights.
// They lay out what they are given and decide nothing.
import Link from "next/link";
import { createContext, useContext, useEffect, useState, type ReactNode } from "react";
import { useRouter } from "next/navigation";
import { useSession } from "@rootxkit/uspace-ui/auth/client";
import { useNowMs, type LiveStatus } from "@rootxkit/uspace-ui/live";
import { useTheme } from "@rootxkit/uspace-ui/theme";
import { DropdownMenuItem, LanguageSwitch, PageHeader, StatusPill, UserMenu, cn, type Crumb, type LinkRenderer } from "@rootxkit/uspace-ui/ui";
import { useRuntimeConfig } from "@/components/Providers";
import { useAppT } from "@/i18n/t";
import { Mark, MoonIcon, SignOutIcon, SunIcon } from "./icons";

/** next/link for the kit's frame components, so navigation stays client-side. */
export const renderLink: LinkRenderer = (p) => <Link {...p} />;

/** The trail of the current page up to its group, set by the frame around it. */
export const CrumbContext = createContext<readonly Crumb[] | null>(null);

/**
 * A page: its section, named by the PageHeader's h1, with the trail the
 * frame set, the description, what is beside the title and the page's
 * actions.
 */
export function PageFrame(props: {
  id: string;
  title: ReactNode;
  description?: ReactNode;
  meta?: ReactNode;
  actions?: ReactNode;
  className?: string;
  children?: ReactNode;
}) {
  const crumbs = useContext(CrumbContext);
  return (
    <section aria-labelledby={props.id} className={cn("flex min-w-0 flex-col gap-4", props.className)}>
      <PageHeader
        title={<span id={props.id}>{props.title}</span>}
        description={props.description}
        meta={props.meta}
        actions={props.actions}
        breadcrumbs={crumbs === null ? undefined : [...crumbs, { label: props.title }]}
        renderLink={renderLink}
      />
      {props.children}
    </section>
  );
}

/** The light/dark switch of the top bar; the choice is kept in the kit's scheme cookie (Providers). */
export function SchemeToggle() {
  const t = useAppT();
  const { resolved, setScheme } = useTheme();
  const dark = resolved === "dark";
  return (
    <button
      type="button"
      aria-pressed={dark}
      aria-label={t("portal.scheme.dark")}
      data-testid="scheme-toggle"
      onClick={() => setScheme(dark ? "light" : "dark")}
      className="inline-flex size-8 shrink-0 items-center justify-center rounded-md border border-border bg-card text-foreground transition-colors hover:bg-hover focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
    >
      {dark ? <MoonIcon /> : <SunIcon />}
    </button>
  );
}

/**
 * The signed-in person in the top bar: the roles the session carries
 * (display only, the API decides every request) and the kit's user menu
 * with sign-out, which leaves for signedOutPath.
 */
export function SessionMenu({ signedOutPath }: { signedOutPath: string }) {
  const t = useAppT();
  const { session, signOut } = useSession();
  const router = useRouter();
  if (session === null) return null;
  const roles = session.roles.join(", ");
  return (
    <div className="flex min-w-0 items-center gap-2" data-testid="session">
      <span className="hidden max-w-56 truncate text-xs text-muted-foreground lg:inline">{t("portal.session.roles", { roles })}</span>
      <UserMenu name={session.sub} detail={roles}>
        <DropdownMenuItem
          onSelect={() => {
            void signOut().then(() => {
              router.push(signedOutPath);
              router.refresh();
            });
          }}
        >
          <SignOutIcon />
          {t("portal.nav.sign_out")}
        </DropdownMenuItem>
      </UserMenu>
    </div>
  );
}

type FeedPillValue = Pick<LiveStatus, "connection" | "lastFrameAtMs"> | null;

const FeedPillContext = createContext<((v: FeedPillValue) => void) | null>(null);

/** Holds the feed of the page shown, for the top bar's pill. */
export function useFeedPillState(): [FeedPillValue, (v: FeedPillValue) => void] {
  return useState<FeedPillValue>(null);
}

export function FeedPillProvider({ set, children }: { set: (v: FeedPillValue) => void; children: ReactNode }) {
  return <FeedPillContext.Provider value={set}>{children}</FeedPillContext.Provider>;
}

/**
 * A page with a live feed shows its state in the top bar while it is
 * shown. A page without one shows no pill rather than one that would say
 * nothing true.
 */
export function useShellFeed(feed: Pick<LiveStatus, "connection" | "lastFrameAtMs">): void {
  const set = useContext(FeedPillContext);
  const { connection, lastFrameAtMs } = feed;
  useEffect(() => {
    set?.({ connection, lastFrameAtMs });
  }, [set, connection, lastFrameAtMs]);
  useEffect(() => () => set?.(null), [set]);
}

/** The pill: the feed's state as the kit's live client reports it, and the age of its last frame. */
export function FeedPill({ value }: { value: FeedPillValue }) {
  const nowMs = useNowMs(1000);
  if (value === null) return null;
  const ageS = value.lastFrameAtMs === null ? null : Math.max(0, Math.round((nowMs - value.lastFrameAtMs) / 1000));
  return <StatusPill state={value.connection} ageS={ageS} />;
}

/** The content column inside the AppShell: the page, then the frame's footer. */
export function ShellContent({ children, footer }: { children: ReactNode; footer: ReactNode }) {
  return (
    <div className="mx-auto flex w-full max-w-[96rem] flex-col gap-6 p-4 sm:p-6">
      {children}
      <footer className="border-t border-border pt-3 text-xs text-muted-foreground">{footer}</footer>
    </div>
  );
}

/**
 * The frame of the sign-in and sign-up pages, and of a page shown signed
 * out: the brand, the scheme and the language, and the page in a column
 * at the centre. No navigation: there is nothing to reach before signing in.
 */
export function AuthFrame({ home, name, wide = false, footer, children }: { home: string; name: ReactNode; wide?: boolean; footer?: ReactNode; children: ReactNode }) {
  const t = useAppT();
  const cfg = useRuntimeConfig();
  return (
    <div className="flex min-h-dvh flex-col bg-background text-foreground">
      <a
        href="#main"
        className="sr-only focus:not-sr-only focus:fixed focus:top-2 focus:left-2 focus:z-50 focus:rounded-md focus:bg-card focus:px-3 focus:py-2 focus:text-sm focus:font-semibold focus:text-foreground focus:shadow-overlay focus:outline-2 focus:outline-ring"
      >
        {t("portal.skip")}
      </a>
      <header className="flex flex-wrap items-center justify-between gap-3 border-b border-border bg-topbar px-4 py-3 sm:px-6">
        <Link href={home} className="flex min-w-0 items-center gap-2.5 rounded-md text-sm font-bold text-foreground no-underline focus-visible:outline-2 focus-visible:outline-ring">
          <Mark />
          <span className="truncate">{name}</span>
        </Link>
        <div className="flex items-center gap-2">
          <SchemeToggle />
          <LanguageSwitch />
        </div>
      </header>
      <main id="main" tabIndex={-1} className="flex flex-1 justify-center px-4 py-8 outline-none sm:py-14">
        <div className={cn("flex w-full flex-col gap-6", wide ? "max-w-2xl" : "max-w-md")}>{children}</div>
      </main>
      <footer className="border-t border-border px-4 py-3 text-xs text-muted-foreground sm:px-6">
        {footer} {t("portal.footer.a11y", { target: cfg.accessibilityTarget, status: cfg.accessibilityStatus })}
      </footer>
    </div>
  );
}
