"use client";

// The DSS panel (brief WP-18, S-M5): this USSP's availability as the DSS
// knows it and since when the DSS is reachable or not, the readiness of
// the DSS dependency with its time and detail, the outbox depth by kind
// with the oldest item and how many retry, the last errors and the
// subscriptions held.
import { useCallback } from "react";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@rootxkit/uspace-ui/ui";
import { ProblemNotice } from "@/components/ProblemNotice";
import { useAppT } from "@/i18n/t";
import { useConsoleApi } from "@/lib/consoleApi";
import { Fact, PageHeading, Utc } from "@/portal/common";
import { body, usePoll } from "./usePoll";

const DSS_PERIOD_MS = 5000;

export function ConsoleDssPage() {
  const t = useAppT();
  const api = useConsoleApi();
  const read = useCallback(() => api.GET("/v1/admin/dss").then(body), [api]);
  const { data, error, failingSinceMs } = usePoll(read, DSS_PERIOD_MS);
  return (
    <section aria-labelledby="dss-heading" className="flex flex-col gap-3">
      <PageHeading id="dss-heading">{t("console.dss.heading")}</PageHeading>
      {failingSinceMs !== null && (
        <p role="alert" className="m-0 font-semibold">
          {t("console.unread_since")} <Utc iso={new Date(failingSinceMs).toISOString()} />
        </p>
      )}
      <ProblemNotice error={error} />
      {data !== null && (
        <>
          <section aria-labelledby="dss-readiness" data-testid="dss-readiness" data-state={data.readiness.state} className="rounded border border-[var(--us-border)] p-3">
            <h2 id="dss-readiness" className="m-0 text-base font-semibold">
              {t("console.dss.readiness")}: {t(`console.dep.${data.readiness.state}`)}
            </h2>
            <p className="m-0 text-sm">
              {t("console.since")} <Utc iso={data.readiness.since} />
            </p>
            {data.readiness.detail !== undefined && <p className="m-0 text-sm" data-testid="dss-detail">{data.readiness.detail}</p>}
          </section>
          <dl className="m-0 grid grid-cols-2 gap-2 md:grid-cols-4" data-testid="dss-state">
            <Fact term={t("console.dss.availability")}>{data.dss_state.uss_availability ?? t("console.none")}</Fact>
            <Fact term={t("console.dss.set")}>
              {data.dss_state.set_by ?? t("console.none")} <Utc iso={data.dss_state.set_at} />
            </Fact>
            <Fact term={t("console.dss.reachable_since")}>
              <Utc iso={data.dss_state.dss_reachable_since} />
            </Fact>
            <Fact term={t("console.dss.unreachable_since")}>
              <Utc iso={data.dss_state.dss_unreachable_since} />
            </Fact>
          </dl>
          <section aria-labelledby="dss-outbox">
            <h2 id="dss-outbox" className="m-0 mb-1 text-base font-semibold">
              {t("console.dss.outbox", { n: data.outbox.pending, retrying: data.outbox.retrying ?? 0 })}
            </h2>
            <ul className="m-0 pl-5 text-sm" data-testid="dss-outbox">
              {Object.entries(data.outbox.by_kind).map(([k, n]) => (
                <li key={k}>
                  {k}: {n}
                </li>
              ))}
            </ul>
            {data.outbox.oldest_created_at !== undefined && (
              <p className="m-0 text-sm">
                {t("console.dss.oldest")} <Utc iso={data.outbox.oldest_created_at} />
              </p>
            )}
          </section>
          <section aria-labelledby="dss-errors">
            <h2 id="dss-errors" className="m-0 mb-1 text-base font-semibold">
              {t("console.dss.errors")}
            </h2>
            {data.last_errors.length === 0 ? (
              <p className="m-0 text-sm">{t("console.dss.no_errors")}</p>
            ) : (
              <Table data-testid="dss-errors">
                <TableHeader>
                  <TableRow>
                    <TableHead>{t("console.dss.kind")}</TableHead>
                    <TableHead>{t("console.dss.entity")}</TableHead>
                    <TableHead>{t("console.dss.attempts")}</TableHead>
                    <TableHead>{t("console.dss.error")}</TableHead>
                    <TableHead>{t("console.dss.next")}</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {data.last_errors.map((e, i) => (
                    <TableRow key={`${e.kind}-${e.entity_id}-${i}`}>
                      <TableCell className="text-xs">{e.kind}</TableCell>
                      <TableCell className="font-mono text-xs">{e.entity_id}</TableCell>
                      <TableCell className="text-xs">{e.attempts}</TableCell>
                      <TableCell className="max-w-md break-words text-xs">{e.last_error}</TableCell>
                      <TableCell className="text-xs">
                        {e.done_at !== undefined ? t("console.dss.done") : <Utc iso={e.next_at} />}
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            )}
          </section>
          <section aria-labelledby="dss-subs">
            <h2 id="dss-subs" className="m-0 mb-1 text-base font-semibold">
              {t("console.dss.subscriptions", { n: data.subscriptions.length })}
            </h2>
            <ul className="m-0 pl-5 text-sm">
              {data.subscriptions.map((s) => (
                <li key={s.subscription_id}>
                  <span className="font-mono">{s.subscription_id}</span> ({s.kind}) {t("console.dss.until")} <Utc iso={s.time_end} />
                </li>
              ))}
            </ul>
          </section>
        </>
      )}
    </section>
  );
}
