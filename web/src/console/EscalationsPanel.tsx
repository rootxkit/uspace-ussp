"use client";

// The escalations the supervisor console must handle (spec 02 F5: an
// unacknowledged critical alert escalates to the supervisor; brief
// WP-18): every alert escalated and not closed, oldest first, with its
// actions. A cleared alert stays until a supervisor closes it. An
// unreadable list says since when it fails, never that there is none.
import { useCallback } from "react";
import { ProblemNotice } from "@/components/ProblemNotice";
import { useAppT } from "@/i18n/t";
import { useConsoleApi } from "@/lib/consoleApi";
import { Utc } from "@/portal/common";
import { ConsoleAlertsTable } from "./AlertsTable";
import { body, usePoll } from "./usePoll";

/** How often the escalations are read. Display-only. */
export const ESCALATIONS_PERIOD_MS = 3000;

export function EscalationsPanel() {
  const t = useAppT();
  const api = useConsoleApi();
  const read = useCallback(() => api.GET("/v1/admin/escalations").then(body), [api]);
  const { data, error, failingSinceMs, reload } = usePoll(read, ESCALATIONS_PERIOD_MS);
  return (
    <section aria-labelledby="escalations-heading" data-testid="escalations" className="rounded border-2 border-[var(--us-severity-critical)] p-3">
      <h2 id="escalations-heading" className="m-0 mb-1 text-base font-semibold">
        {t("console.escalations.heading", { n: data?.alerts.length ?? 0 })}
      </h2>
      {data !== null && (
        <p className="m-0 mb-1 text-xs text-[var(--us-text-muted)]">
          {t("console.escalations.note", { after: data.escalation_after_s, repeat: data.escalation_repeat_s, version: data.policy_version })}
        </p>
      )}
      {failingSinceMs !== null && (
        <p role="alert" className="m-0 text-sm font-semibold">
          {t("console.unread_since")} <Utc iso={new Date(failingSinceMs).toISOString()} />
        </p>
      )}
      <ProblemNotice error={error} testId="escalations-problem" />
      {data !== null && <ConsoleAlertsTable alerts={data.alerts} testId="escalations-table" onChanged={reload} />}
      {data?.truncated === true && <p className="m-0">{t("portal.list.truncated")}</p>}
    </section>
  );
}
