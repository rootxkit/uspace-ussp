"use client";

// The alerts of the record as the console shows them (brief WP-18; spec
// 02 F5, Art. 13): severity, kind with the kit's summary and the numbers
// as sent, the flight and intent, the state with its clear reason, when
// raised and updated, the acknowledgement (when and by whom), how many
// messages of it were recorded, the escalation (automatic after
// escalation_after_s, or a supervisor's with a reason) and the console's
// close. A supervisor escalates, closes (which never clears an alert:
// the monitor alone does) or reports it as an occurrence, each with a
// reason the audit keeps; every row's audit is one click away. Nothing
// here advises or reaches an aircraft.
import { useState } from "react";
import { RequireRole } from "@rootxkit/uspace-ui/auth/client";
import { SeverityMark, alertSummary, detailNumber, kindName } from "@rootxkit/uspace-ui/alerts";
import { useLang, useT } from "@rootxkit/uspace-ui/i18n";
import { isSeverity } from "@rootxkit/uspace-ui/model";
import { Button, Input, Label, Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@rootxkit/uspace-ui/ui";
import type { components } from "@/api/generated/openapi";
import { ProblemNotice } from "@/components/ProblemNotice";
import { useAppT } from "@/i18n/t";
import { useConsoleApi } from "@/lib/consoleApi";
import { Num, Utc } from "@/portal/common";
import { adaptAlertBody, kitAlertView } from "@/portal/live/adapt";
import { AuditRows } from "./AuditRows";

export type AdminAlert = components["schemas"]["AdminAlert"];

function summaryOf(a: AdminAlert, kitT: ReturnType<typeof useT>, lang: ReturnType<typeof useLang>["lang"]): string | null {
  // The record's alert in alert/v1's shape, for the kit's one line.
  const p = adaptAlertBody({
    alert_id: a.alert_id, kind: a.kind, severity: a.severity, state: a.state, captured_at: a.updated_at, raised_at: a.raised_at,
    policy_version: a.policy_version, detail: a.detail, flight_id: a.flight_id ?? null, acked_at: a.acked_at ?? null,
  });
  const v = p === null ? null : kitAlertView(p);
  return v === null ? null : alertSummary(v, kitT, lang);
}

function Row({ a, onChanged, idPrefix }: { a: AdminAlert; onChanged(): void; idPrefix: string }) {
  const t = useAppT();
  const kitT = useT();
  const { lang } = useLang();
  const api = useConsoleApi();
  const [reason, setReason] = useState("");
  const [error, setError] = useState<unknown>(null);
  const [done, setDone] = useState<string | null>(null);
  const [audit, setAudit] = useState(false);
  const act = (what: "escalate" | "close") => {
    setError(null);
    const path = what === "escalate" ? "/v1/admin/alerts/{alert_id}/escalate" : "/v1/admin/alerts/{alert_id}/close";
    api
      .POST(path, { params: { path: { alert_id: a.alert_id } }, body: { reason } })
      .then(() => {
        setReason("");
        setDone(what);
        onChanged();
      })
      .catch(setError);
  };
  const report = () => {
    setError(null);
    api
      .POST("/v1/admin/occurrences", { body: { alert_id: a.alert_id, ...(reason === "" ? {} : { narrative: reason }) } })
      .then(() => {
        setReason("");
        setDone("occurrence");
      })
      .catch(setError);
  };
  const summary = summaryOf(a, kitT, lang);
  // One alert may be on two tables of a page (the escalations and the list): ids per table.
  const reasonId = `${idPrefix}-reason-${a.alert_id}`;
  return (
    <TableRow
      data-testid="admin-alert-row"
      data-alert-id={a.alert_id}
      data-kind={a.kind}
      data-intent-id={a.intent_id ?? ""}
      data-state={a.state}
      data-escalated={a.escalated_at !== undefined ? "true" : "false"}
      data-closed={a.closed_at !== undefined ? "true" : "false"}
    >
      <TableCell>{isSeverity(a.severity) ? <SeverityMark severity={a.severity} /> : a.severity}</TableCell>
      <TableCell>
        <p className="m-0 font-semibold">{kindName(kitT, a.kind)}</p>
        {summary !== null && <p className="m-0 text-xs">{summary}</p>}
        <dl className="m-0 grid grid-cols-[auto_auto] gap-x-2 text-xs">
          <dt>{t("portal.alerts.d_cpa_h")}</dt>
          <dd className="m-0">
            <Num v={detailNumber(a.detail, "d_cpa_h_m")} unit="m" />
          </dd>
          <dt>{t("portal.alerts.d_alt")}</dt>
          <dd className="m-0">
            <Num v={detailNumber(a.detail, "d_alt_m")} unit="m" />
          </dd>
        </dl>
        <p className="m-0 font-mono text-xs">{a.alert_id}</p>
      </TableCell>
      <TableCell className="text-xs">
        <p className="m-0">
          {t("console.alerts.flight")} <span className="font-mono">{a.flight_id ?? t("console.none")}</span>
        </p>
        {a.authorisation_number !== undefined && <p className="m-0">{a.authorisation_number}</p>}
      </TableCell>
      <TableCell className="text-xs">
        {t(`portal.alerts.state.${a.state as "raised" | "updated" | "cleared"}`)}
        {a.clear_reason !== undefined && ` (${a.clear_reason})`}
        <p className="m-0" data-testid="messages-recorded">
          {t("console.alerts.messages", { n: a.messages_recorded })}
        </p>
        <p className="m-0">
          {t("portal.alerts.raised")} <Utc iso={a.raised_at} />
        </p>
        <p className="m-0">
          {t("portal.alerts.updated")} <Utc iso={a.updated_at} />
        </p>
      </TableCell>
      <TableCell className="text-xs">
        {a.acked_at !== undefined ? (
          <p className="m-0" data-testid="admin-acked">
            {t("console.alerts.acked_by", { who: a.acked_by ?? "" })} <Utc iso={a.acked_at} />
          </p>
        ) : (
          <p className="m-0">{t("portal.alerts.not_acked")}</p>
        )}
      </TableCell>
      <TableCell className="text-xs">
        {a.escalated_at !== undefined ? (
          <div data-testid="admin-escalated">
            <p className="m-0 font-semibold">
              {t("portal.alerts.escalated")} <Utc iso={a.escalated_at} />
            </p>
            <p className="m-0">{a.escalated_by === undefined ? t("console.alerts.escalated_auto") : t("console.alerts.by", { who: a.escalated_by })}</p>
            {a.escalation_reason !== undefined && <p className="m-0">{a.escalation_reason}</p>}
          </div>
        ) : (
          t("console.alerts.not_escalated")
        )}
        {a.closed_at !== undefined && (
          <div data-testid="admin-closed">
            <p className="m-0 font-semibold">
              {t("console.alerts.closed")} <Utc iso={a.closed_at} />
            </p>
            <p className="m-0">
              {t("console.alerts.by", { who: a.closed_by ?? "" })}: {a.close_reason}
            </p>
          </div>
        )}
      </TableCell>
      <TableCell className="text-xs">
        <RequireRole anyOf={["supervisor"]} fallback={t("console.read_only")}>
          <div className="flex flex-col gap-1">
            <Label htmlFor={reasonId}>{t("console.reason")}</Label>
            <Input id={reasonId} value={reason} maxLength={500} onChange={(e) => setReason(e.target.value)} />
            <div className="flex flex-wrap gap-1">
              {a.escalated_at === undefined && (
                <Button size="sm" disabled={reason === ""} onClick={() => act("escalate")}>
                  {t("console.alerts.escalate")}
                </Button>
              )}
              {a.closed_at === undefined && (
                <Button size="sm" variant="outline" disabled={reason === ""} onClick={() => act("close")}>
                  {t("console.alerts.close")}
                </Button>
              )}
              <Button size="sm" variant="outline" onClick={report}>
                {t("console.alerts.report")}
              </Button>
            </div>
            {done !== null && (
              <p role="status" className="m-0" data-testid="alert-done">
                {t(`console.alerts.done.${done as "escalate" | "close" | "occurrence"}`)}
              </p>
            )}
            <ProblemNotice error={error} testId="alert-problem" />
          </div>
        </RequireRole>
        <Button size="sm" variant="link" aria-expanded={audit} onClick={() => setAudit((v) => !v)}>
          {t("console.audit.show")}
        </Button>
        {audit && <AuditRows entityType="alert" entityId={a.alert_id} />}
      </TableCell>
    </TableRow>
  );
}

export function ConsoleAlertsTable({ alerts, testId, onChanged }: { alerts: readonly AdminAlert[]; testId: string; onChanged(): void }) {
  const t = useAppT();
  if (alerts.length === 0) {
    return (
      <p role="status" data-testid={`${testId}-none`}>
        {t("portal.alerts.none")}
      </p>
    );
  }
  return (
    <Table data-testid={testId}>
      <TableHeader>
        <TableRow>
          <TableHead>{t("portal.alerts.severity")}</TableHead>
          <TableHead>{t("portal.alerts.kind")}</TableHead>
          <TableHead>{t("console.alerts.flight")}</TableHead>
          <TableHead>{t("portal.alerts.state")}</TableHead>
          <TableHead>{t("portal.alerts.acknowledgement")}</TableHead>
          <TableHead>{t("console.alerts.escalation")}</TableHead>
          <TableHead>{t("console.actions")}</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {alerts.map((a) => (
          <Row key={a.alert_id} a={a} onChanged={onChanged} idPrefix={testId} />
        ))}
      </TableBody>
    </Table>
  );
}
