"use client";

// The console's alerts page (brief WP-18): the alerts of the record,
// active or with the ones cleared in the last day, each with its
// acknowledgement, the messages recorded, its escalation and close; the
// escalations to handle above them.
import { useCallback, useState } from "react";
import { Button } from "@rootxkit/uspace-ui/ui";
import { ProblemNotice } from "@/components/ProblemNotice";
import { useAppT } from "@/i18n/t";
import { useConsoleApi } from "@/lib/consoleApi";
import { PageHeading, Utc } from "@/portal/common";
import { ConsoleAlertsTable } from "./AlertsTable";
import { EscalationsPanel } from "./EscalationsPanel";
import { body, usePoll } from "./usePoll";

/** How often the alerts are read. Display-only. */
const ALERTS_PERIOD_MS = 3000;

export function ConsoleAlertsPage() {
  const t = useAppT();
  const api = useConsoleApi();
  const [recent, setRecent] = useState(false);
  const read = useCallback(() => api.GET("/v1/admin/alerts", { params: { query: { view: recent ? "recent" : "active" } } }).then(body), [api, recent]);
  const { data, error, failingSinceMs, reload } = usePoll(read, ALERTS_PERIOD_MS);
  return (
    <section aria-labelledby="console-alerts-heading" className="flex flex-col gap-3">
      <PageHeading id="console-alerts-heading">{t("console.alerts.heading")}</PageHeading>
      <EscalationsPanel />
      <div role="group" aria-label={t("console.alerts.view")} className="flex gap-2">
        <Button size="sm" variant={recent ? "outline" : "default"} aria-pressed={!recent} onClick={() => setRecent(false)}>
          {t("console.alerts.view_active")}
        </Button>
        <Button size="sm" variant={recent ? "default" : "outline"} aria-pressed={recent} onClick={() => setRecent(true)}>
          {t("console.alerts.view_recent")}
        </Button>
      </div>
      {failingSinceMs !== null && (
        <p role="alert" className="m-0 font-semibold">
          {t("console.unread_since")} <Utc iso={new Date(failingSinceMs).toISOString()} />
        </p>
      )}
      <ProblemNotice error={error} />
      {data !== null && <ConsoleAlertsTable alerts={data.alerts} testId="console-alerts" onChanged={reload} />}
      {data?.truncated === true && <p className="m-0">{t("portal.list.truncated")}</p>}
    </section>
  );
}
