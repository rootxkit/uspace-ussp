"use client";

// The occurrence reports and the service records of the console (brief
// WP-18; spec 02 F7, Reg. (EU) 376/2014 Art. 4(8), Art. 15(1)(g)): every
// report not delivered with its 72 h deadline and the time left (past it
// is critical), its tries and last error, and why nothing is sent when
// the authority publishes no route; a supervisor reports an alert as an
// occurrence. The daily record bundles of the last days and every day
// without one, said, never left out.
import { useCallback, useState } from "react";
import { RequireRole } from "@rootxkit/uspace-ui/auth/client";
import { Button, Input, Label, Table, TableBody, TableCell, TableHead, TableHeader, TableRow, Textarea } from "@rootxkit/uspace-ui/ui";
import { ProblemNotice } from "@/components/ProblemNotice";
import { useAppT } from "@/i18n/t";
import { useConsoleApi } from "@/lib/consoleApi";
import { Num, PageHeading, Utc } from "@/portal/common";
import { body, usePoll } from "./usePoll";

const PERIOD_MS = 10000;

function FlagForm({ onFlagged }: { onFlagged(): void }) {
  const t = useAppT();
  const api = useConsoleApi();
  const [alertId, setAlertId] = useState("");
  const [narrative, setNarrative] = useState("");
  const [error, setError] = useState<unknown>(null);
  const [ref, setRef] = useState<string | null>(null);
  return (
    <RequireRole anyOf={["supervisor"]} fallback={<p className="m-0 text-sm">{t("console.read_only")}</p>}>
      <form
        className="flex max-w-xl flex-col gap-2"
        data-testid="occurrence-flag"
        onSubmit={(e) => {
          e.preventDefault();
          setError(null);
          api
            .POST("/v1/admin/occurrences", { body: { alert_id: alertId, ...(narrative === "" ? {} : { narrative }) } })
            .then(({ data }) => {
              setRef(data?.report_ref ?? null);
              setAlertId("");
              setNarrative("");
              onFlagged();
            })
            .catch(setError);
        }}
      >
        <h2 className="m-0 text-base font-semibold">{t("console.occurrences.flag_heading")}</h2>
        <Label htmlFor="occ-alert">{t("console.occurrences.alert")}</Label>
        <Input id="occ-alert" value={alertId} onChange={(e) => setAlertId(e.target.value.trim())} />
        <Label htmlFor="occ-narrative">{t("console.occurrences.narrative")}</Label>
        <Textarea id="occ-narrative" value={narrative} maxLength={4000} onChange={(e) => setNarrative(e.target.value)} />
        <Button type="submit" disabled={alertId === ""}>
          {t("console.occurrences.flag")}
        </Button>
        {ref !== null && (
          <p role="status" className="m-0 text-sm">
            {t("console.occurrences.flagged", { ref })}
          </p>
        )}
        <ProblemNotice error={error} testId="occurrence-problem" />
      </form>
    </RequireRole>
  );
}

export function ConsoleOccurrencesPage() {
  const t = useAppT();
  const api = useConsoleApi();
  const read = useCallback(() => api.GET("/v1/admin/occurrences").then(body), [api]);
  const { data, error, reload } = usePoll(read, PERIOD_MS);
  return (
    <section aria-labelledby="occurrences-heading" className="flex flex-col gap-3">
      <PageHeading id="occurrences-heading">{t("console.occurrences.heading")}</PageHeading>
      <ProblemNotice error={error} />
      {data?.delivery !== undefined && data.delivery !== null && (
        <p role="alert" className="m-0 text-sm" data-testid="occurrences-delivery">
          {data.delivery}
        </p>
      )}
      {data !== null && data.reports.length === 0 && <p role="status">{t("console.occurrences.none")}</p>}
      {data !== null && data.reports.length > 0 && (
        <Table data-testid="occurrences">
          <TableHeader>
            <TableRow>
              <TableHead>{t("console.occurrences.report")}</TableHead>
              <TableHead>{t("console.occurrences.state")}</TableHead>
              <TableHead>{t("console.occurrences.deadline")}</TableHead>
              <TableHead>{t("console.occurrences.tries")}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {data.reports.map((r) => (
              <TableRow key={r.report_ref} data-testid="occurrence-row" data-critical={r.critical ? "true" : "false"}>
                <TableCell className="text-xs">
                  <p className="m-0 font-mono">{r.report_ref}</p>
                  <p className="m-0">
                    {r.kind} · {r.channel} · {r.flagged_by}
                  </p>
                </TableCell>
                <TableCell className="text-xs">{r.state}</TableCell>
                <TableCell className={`text-xs ${r.critical ? "font-semibold" : ""}`}>
                  <Utc iso={r.deadline_at} /> ({t("console.occurrences.left")} <Num v={r.time_to_deadline_s / 3600} digits={1} unit="h" />)
                  {r.critical && <p className="m-0">{t("console.occurrences.critical")}</p>}
                </TableCell>
                <TableCell className="text-xs">
                  {r.attempts}
                  {r.last_error !== null && <p className="m-0 break-words">{r.last_error}</p>}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
      {data?.truncated === true && <p className="m-0">{t("portal.list.truncated")}</p>}
      <FlagForm onFlagged={reload} />
    </section>
  );
}

export function ConsoleRecordsPage() {
  const t = useAppT();
  const api = useConsoleApi();
  const read = useCallback(() => api.GET("/v1/admin/records/days").then(body), [api]);
  const { data, error } = usePoll(read, PERIOD_MS * 6);
  return (
    <section aria-labelledby="records-heading" className="flex flex-col gap-3">
      <PageHeading id="records-heading">{t("console.records.heading")}</PageHeading>
      <p className="m-0 text-sm text-[var(--us-text-muted)]">{t("console.records.note")}</p>
      <ProblemNotice error={error} />
      {data !== null && (
        <>
          {data.missing.length > 0 && (
            <p role="alert" className="m-0 text-sm" data-testid="records-missing">
              {t("console.records.missing", { days: data.missing.join(", ") })}
            </p>
          )}
          {data.days.length === 0 ? (
            <p role="status">{t("console.records.none")}</p>
          ) : (
            <Table data-testid="record-days">
              <TableHeader>
                <TableRow>
                  <TableHead>{t("console.records.date")}</TableHead>
                  <TableHead>{t("console.records.built")}</TableHead>
                  <TableHead>{t("console.records.flights")}</TableHead>
                  <TableHead>{t("console.records.hash")}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {data.days.map((d) => (
                  <TableRow key={d.date}>
                    <TableCell className="text-xs">{d.date}</TableCell>
                    <TableCell className="text-xs">
                      <Utc iso={d.built_at} />
                    </TableCell>
                    <TableCell className="text-xs">{d.flights}</TableCell>
                    <TableCell className="break-all font-mono text-xs">{d.content_hash}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </>
      )}
    </section>
  );
}
