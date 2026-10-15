"use client";

// The escalations the supervisor console must handle (spec 02 F5: an
// unacknowledged critical alert escalates to the supervisor; brief
// WP-18): every alert escalated and not closed, oldest first, with its
// actions. A cleared alert stays until a supervisor closes it. An
// unreadable list says since when it fails, never that there is none.
// The overview counts what this panel read (onData), so the list is read
// once per page.
import { useCallback, useEffect } from "react";
import { Panel } from "@rootxkit/uspace-ui/ui";
import type { components } from "@/api/generated/openapi";
import { ProblemNotice } from "@/components/ProblemNotice";
import { useAppT } from "@/i18n/t";
import { useConsoleApi } from "@/lib/consoleApi";
import { Utc } from "@/portal/common";
import { ConsoleAlertsTable } from "./AlertsTable";
import { body, usePoll } from "./usePoll";

/** How often the escalations are read. Display-only. */
export const ESCALATIONS_PERIOD_MS = 3000;

export type Escalations = components["schemas"]["AdminAlerts"];

export function EscalationsPanel({ onData }: { onData?(data: Escalations | null, failingSinceMs: number | null): void }) {
  const t = useAppT();
  const api = useConsoleApi();
  const read = useCallback(() => api.GET("/v1/admin/escalations").then(body), [api]);
  const { data, error, failingSinceMs, reload } = usePoll(read, ESCALATIONS_PERIOD_MS);
  useEffect(() => {
    onData?.(data, failingSinceMs);
  }, [onData, data, failingSinceMs]);
  return (
    <Panel
      data-testid="escalations"
      className="border-status-critical-border"
      title={t("console.escalations.heading", { n: data?.alerts.length ?? 0 })}
      description={
        data === null ? undefined : t("console.escalations.note", { after: data.escalation_after_s, repeat: data.escalation_repeat_s, version: data.policy_version })
      }
      bodyClassName="flex flex-col gap-2"
    >
      {failingSinceMs !== null && (
        <p role="alert" className="m-0 text-sm font-semibold">
          {t("console.unread_since")} <Utc iso={new Date(failingSinceMs).toISOString()} />
        </p>
      )}
      <ProblemNotice error={error} testId="escalations-problem" />
      {data !== null && <ConsoleAlertsTable alerts={data.alerts} testId="escalations-table" onChanged={reload} />}
      {data?.truncated === true && <p className="m-0">{t("portal.list.truncated")}</p>}
    </Panel>
  );
}
