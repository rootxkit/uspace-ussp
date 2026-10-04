"use client";

// Alerts as the streams send them (alert/v1): severity, kind, the kit's
// one-line summary in the person's language, the numbers of the detail
// as sent (time to and distance at the closest point, the vertical
// separation, the distance now) and the other aircraft with its trust
// class, the state with its clear reason, when it was raised, updated,
// acknowledged and escalated, and how many times it arrived (the alert
// stream repeats an unacknowledged critical alert). An operator_admin or
// a remote_pilot acknowledges it (POST /v1/alerts/{id}/ack). There is no
// advice here and no automation: the remote pilot decides (Art. 11(4),
// LESSONS X-15).
import { useState } from "react";
import { RequireRole } from "@rootxkit/uspace-ui/auth/client";
import { SeverityMark, alertSummary, detailNumber, kindName } from "@rootxkit/uspace-ui/alerts";
import { useLang, useT } from "@rootxkit/uspace-ui/i18n";
import { Button, Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@rootxkit/uspace-ui/ui";
import { ProblemNotice } from "@/components/ProblemNotice";
import { useAppT } from "@/i18n/t";
import { useApi } from "@/lib/api";
import { Num, Utc } from "../common";
import { kitAlertView, type PortalAlert } from "./adapt";

export interface ShownAlert extends PortalAlert {
  received?: number;
}

function sorted(alerts: readonly ShownAlert[]): ShownAlert[] {
  return [...alerts].sort(
    (a, b) => Number(a.state === "cleared") - Number(b.state === "cleared") || b.raisedAt.localeCompare(a.raisedAt) || a.alertId.localeCompare(b.alertId),
  );
}

export function AlertsTable({ alerts, testId = "alerts", onAcked }: { alerts: readonly ShownAlert[]; testId?: string; onAcked?(id: string, at: string): void }) {
  const t = useAppT();
  const kitT = useT();
  const { lang } = useLang();
  const api = useApi();
  const [error, setError] = useState<unknown>(null);
  const [acked, setAcked] = useState<ReadonlyMap<string, string>>(new Map());
  const list = sorted(alerts);
  if (list.length === 0) {
    return (
      <p role="status" data-testid={`${testId}-none`}>
        {t("portal.alerts.none")}
      </p>
    );
  }
  return (
    <>
      <ProblemNotice error={error} testId={`${testId}-problem`} />
      <Table data-testid={testId}>
        <TableHeader>
          <TableRow>
            <TableHead>{t("portal.alerts.severity")}</TableHead>
            <TableHead>{t("portal.alerts.kind")}</TableHead>
            <TableHead>{t("portal.alerts.numbers")}</TableHead>
            <TableHead>{t("portal.alerts.peer")}</TableHead>
            <TableHead>{t("portal.alerts.state")}</TableHead>
            <TableHead>{t("portal.alerts.times")}</TableHead>
            <TableHead>{t("portal.alerts.acknowledgement")}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {list.map((a) => {
            const view = kitAlertView(a);
            const ackedAt = a.ackedAt ?? acked.get(a.alertId) ?? null;
            return (
              <TableRow key={a.alertId} data-testid="alert-row" data-kind={a.kind} data-alert-id={a.alertId} data-state={a.state} data-acked={ackedAt !== null ? "true" : "false"}>
                <TableCell>
                  <SeverityMark severity={a.severity} />
                </TableCell>
                <TableCell>
                  <p className="m-0 font-semibold">{kindName(kitT, a.kind)}</p>
                  {view !== null && <p className="m-0 text-xs">{alertSummary(view, kitT, lang)}</p>}
                  <p className="m-0 font-mono text-xs">{a.alertId}</p>
                </TableCell>
                <TableCell>
                  <dl className="m-0 grid grid-cols-[auto_auto] gap-x-2 text-xs">
                    <dt>{t("portal.alerts.t_cpa")}</dt>
                    <dd className="m-0">
                      <Num v={detailNumber(a.detail, "t_cpa_s")} unit="s" />
                    </dd>
                    <dt>{t("portal.alerts.d_cpa_h")}</dt>
                    <dd className="m-0">
                      <Num v={detailNumber(a.detail, "d_cpa_h_m")} unit="m" />
                    </dd>
                    <dt>{t("portal.alerts.d_alt")}</dt>
                    <dd className="m-0">
                      <Num v={detailNumber(a.detail, "d_alt_m")} unit="m" />
                    </dd>
                    <dt>{t("portal.alerts.d_now")}</dt>
                    <dd className="m-0">
                      <Num v={detailNumber(a.detail, "d_horizontal_now_m")} unit="m" />
                    </dd>
                  </dl>
                </TableCell>
                <TableCell className="text-xs">
                  {a.peerTrackId === null ? (
                    t("portal.alerts.no_peer")
                  ) : (
                    <>
                      <span className="font-mono">{a.peerTrackId}</span>
                      {a.peerTrust !== null && <span> ({kitT(`trust.${a.peerTrust}`)})</span>}
                    </>
                  )}
                </TableCell>
                <TableCell className="text-xs">
                  {t(`portal.alerts.state.${a.state}`)}
                  {a.clearReason !== null && ` (${a.clearReason})`}
                  {a.received !== undefined && a.received > 1 && <p className="m-0">{t("portal.alerts.received", { n: a.received })}</p>}
                  {a.escalatedAt !== null && (
                    <p className="m-0 font-semibold" data-testid="escalated">
                      {t("portal.alerts.escalated")} <Utc iso={a.escalatedAt} />
                    </p>
                  )}
                </TableCell>
                <TableCell className="text-xs">
                  <p className="m-0">
                    {t("portal.alerts.raised")} <Utc iso={a.raisedAt} />
                  </p>
                  <p className="m-0">
                    {t("portal.alerts.updated")} <Utc iso={a.updatedAt} />
                  </p>
                </TableCell>
                <TableCell className="text-xs">
                  {ackedAt !== null ? (
                    <p className="m-0" data-testid="acked">
                      {t("portal.alerts.acked")} <Utc iso={ackedAt} />
                    </p>
                  ) : a.state === "cleared" ? (
                    ""
                  ) : (
                    <RequireRole anyOf={["operator_admin", "remote_pilot"]} fallback={t("portal.alerts.not_acked")}>
                      <Button
                        size="sm"
                        onClick={() => {
                          setError(null);
                          api
                            .POST("/v1/alerts/{alert_id}/ack", { params: { path: { alert_id: a.alertId } } })
                            .then(({ data }) => {
                              if (data === undefined) return;
                              setAcked((m) => new Map(m).set(a.alertId, data.acked_at));
                              onAcked?.(a.alertId, data.acked_at);
                            })
                            .catch(setError);
                        }}
                      >
                        {t("portal.alerts.acknowledge")}
                      </Button>
                    </RequireRole>
                  )}
                </TableCell>
              </TableRow>
            );
          })}
        </TableBody>
      </Table>
    </>
  );
}
