"use client";

// Small pieces every portal page uses: the signed-out notice, a value
// with its unit, a time in UTC, and the "not given" dash. They format
// what the API said; none computes anything.
import type { ReactNode } from "react";
import Link from "next/link";
import { useSession } from "@rootxkit/uspace-ui/auth/client";
import { DASH, fmtNum, fmtTimeUTC, useLang } from "@rootxkit/uspace-ui/i18n";
import { useAppT } from "@/i18n/t";

/** Children for a portal session; otherwise the way to sign in. */
export function RequireSession({ children }: { children: ReactNode }) {
  const t = useAppT();
  const { session } = useSession();
  if (session === null) {
    return (
      <p role="status" data-testid="signed-out">
        {t("portal.signed_out")}{" "}
        <Link href="/login" className="underline">
          {t("portal.nav.login")}
        </Link>
      </p>
    );
  }
  return <>{children}</>;
}

/** A number with its unit in the person's language, or a dash. */
export function Num({ v, digits = 1, unit }: { v: number | null | undefined; digits?: number; unit?: string }) {
  const { lang } = useLang();
  return <>{v === null || v === undefined ? DASH : fmtNum(v, digits, unit, lang)}</>;
}

/** An instant in UTC with the seconds, or a dash. */
export function Utc({ iso }: { iso: string | null | undefined }) {
  const { lang } = useLang();
  return <time dateTime={iso ?? undefined}>{iso === null || iso === undefined ? DASH : fmtTimeUTC(iso, lang, { seconds: true })}</time>;
}

/** A page heading. */
export function PageHeading({ id, children }: { id: string; children: ReactNode }) {
  return (
    <h1 id={id} className="m-0 text-xl font-bold">
      {children}
    </h1>
  );
}

/** A term and its value in a definition list. */
export function Fact({ term, children, testId }: { term: string; children: ReactNode; testId?: string }) {
  return (
    <div className="flex flex-col" data-testid={testId}>
      <dt className="text-xs text-[var(--us-text-muted)]">{term}</dt>
      <dd className="m-0 break-words">{children}</dd>
    </div>
  );
}
