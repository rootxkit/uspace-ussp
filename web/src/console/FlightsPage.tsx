"use client";

// The active flights (brief WP-18, S-M5): each with its intent and the
// intent's local and DSS state, the newest conformance state with its
// time, the client its samples come from (operator_ws's instance), the
// last sample's time and age from the telemetry record (or why it is
// not shown: the record unavailable since T is not an absence of
// telemetry), the emergency flag and the emergency workflow.
import Link from "next/link";
import { useCallback } from "react";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@rootxkit/uspace-ui/ui";
import { ProblemNotice } from "@/components/ProblemNotice";
import { useAppT } from "@/i18n/t";
import { useConsoleApi } from "@/lib/consoleApi";
import { Num, PageHeading, Utc } from "@/portal/common";
import { body, usePoll } from "./usePoll";

const FLIGHTS_PERIOD_MS = 5000;

export function ConsoleFlightsPage() {
  const t = useAppT();
  const api = useConsoleApi();
  const read = useCallback(() => api.GET("/v1/admin/flights").then(body), [api]);
  const { data, error, failingSinceMs } = usePoll(read, FLIGHTS_PERIOD_MS);
  return (
    <section aria-labelledby="flights-heading" className="flex flex-col gap-3">
      <PageHeading id="flights-heading">{t("console.flights.heading")}</PageHeading>
      {failingSinceMs !== null && (
        <p role="alert" className="m-0 font-semibold">
          {t("console.unread_since")} <Utc iso={new Date(failingSinceMs).toISOString()} />
        </p>
      )}
      <ProblemNotice error={error} />
      {data !== null && !data.samples.available && (
        <p role="alert" className="m-0" data-testid="samples-unavailable">
          {data.samples.detail}
        </p>
      )}
      {data !== null && data.flights.length === 0 && (
        <p role="status" data-testid="no-flights">
          {t("console.flights.none")}
        </p>
      )}
      {data !== null && data.flights.length > 0 && (
        <Table data-testid="console-flights">
          <TableHeader>
            <TableRow>
              <TableHead>{t("console.flights.flight")}</TableHead>
              <TableHead>{t("console.flights.intent")}</TableHead>
              <TableHead>{t("console.flights.conformance")}</TableHead>
              <TableHead>{t("console.flights.source")}</TableHead>
              <TableHead>{t("console.flights.last_sample")}</TableHead>
              <TableHead>{t("console.flights.emergency")}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {data.flights.map((f) => (
              <TableRow key={f.flight_id} data-testid="flight-row" data-flight-id={f.flight_id} data-serial={f.uas_serial}>
                <TableCell className="text-xs">
                  <p className="m-0 font-mono">{f.flight_id}</p>
                  <p className="m-0">{f.uas_serial}</p>
                  <p className="m-0">
                    {t("console.flights.started")} <Utc iso={f.started_at} />
                  </p>
                </TableCell>
                <TableCell className="text-xs">
                  {f.intent_id === undefined ? (
                    t("console.flights.no_intent")
                  ) : (
                    <>
                      <p className="m-0 font-mono">{f.intent_id}</p>
                      <p className="m-0">{f.authorisation_number}</p>
                      <p className="m-0">
                        {f.intent_state ?? t("console.none")} · DSS {f.dss_state ?? t("console.none")}
                      </p>
                    </>
                  )}
                </TableCell>
                <TableCell className="text-xs">
                  {f.conformance === undefined ? (
                    t("console.flights.no_conformance")
                  ) : (
                    <>
                      <p className="m-0 font-semibold">{f.conformance.state}</p>
                      {f.conformance.reason !== undefined && <p className="m-0">{f.conformance.reason}</p>}
                      <Utc iso={f.conformance.at} />
                    </>
                  )}
                </TableCell>
                <TableCell className="font-mono text-xs">{f.client_id ?? t("console.none")}</TableCell>
                <TableCell className="text-xs">
                  {f.last_sample_at === undefined ? (
                    t("console.flights.no_sample")
                  ) : (
                    <>
                      <Utc iso={f.last_sample_at} /> (<Num v={f.last_sample_age_s} unit="s" />)
                    </>
                  )}
                </TableCell>
                <TableCell className="text-xs">
                  {f.emergency && <p className="m-0 font-semibold">{t("portal.traffic.emergency")}</p>}
                  <Link href={`/console/emergency/${f.flight_id}`} className="underline" data-testid="flight-emergency-link">
                    {f.emergency_case_open ? t("console.flights.case_open") : t("console.flights.workflow")}
                  </Link>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
      {data?.truncated === true && <p className="m-0">{t("portal.list.truncated")}</p>}
    </section>
  );
}
