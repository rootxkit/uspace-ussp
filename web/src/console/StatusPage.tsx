"use client";

// The operations-status page (Reg. (EU) 2021/664 Art. 7(6); api's
// /v1/admin/status): the state the notices give (not started, operating,
// ceased), the certificate they name, and every notice with its delivery
// to the authority (pending with its tries, last error and next try,
// delivered with the authority's notice id, failed). An admin confirms
// the start of operations, ceases them or restarts them, each behind a
// dialog saying that it notifies the civil aviation authority and
// changes the certificate's status; a supervisor or a support viewer
// reads. Only the transition the notices admit is offered; the API
// decides every request (admin only, 400 for a transition it does not
// admit) and the page shows its answer. Nothing here reaches an
// aircraft.
import { useCallback, useState } from "react";
import { RequireRole } from "@rootxkit/uspace-ui/auth/client";
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogTrigger,
  Button,
  StatusBadge,
  TABLE_NOWRAP,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@rootxkit/uspace-ui/ui";
import type { components } from "@/api/generated/openapi";
import { PageFrame } from "@/components/frame";
import { ProblemNotice } from "@/components/ProblemNotice";
import { useAppT } from "@/i18n/t";
import { useConsoleApi } from "@/lib/consoleApi";
import { Fact, Utc } from "@/portal/common";
import { body, usePoll } from "./usePoll";

type Notice = components["schemas"]["StatusNotice"];
type Kind = Notice["kind"];
type Delivery = Notice["state"];
export type OperatingState = "not_started" | "operating" | "ceased";

/** A display period, not a threshold: a pending notice's delivery shows within it. */
const STATUS_PERIOD_MS = 5000;

/**
 * The newest notice that has not failed (the notices come newest first),
 * the one api's requestStatusNotice follows: a failed notice changed
 * nothing at the authority.
 */
function standing(notices: readonly Notice[]): Notice | undefined {
  return notices.find((n) => n.state !== "failed");
}

/** The state the standing notice gives: none is not started, a start or a restart is operating, a cease is ceased. */
function stateOf(n: Notice | undefined): OperatingState {
  if (n === undefined) return "not_started";
  return n.kind === "cease" ? "ceased" : "operating";
}

/** The one notice the API admits from a state: a start before any, a cease while operating, a restart after a cease. */
const NEXT: Record<OperatingState, Kind> = { not_started: "start", operating: "cease", ceased: "restart" };

const STATE_TONE: Record<OperatingState, "ok" | "warning" | "neutral"> = { not_started: "warning", operating: "ok", ceased: "neutral" };
const DELIVERY_TONE: Record<Delivery, "ok" | "info" | "critical"> = { pending: "info", delivered: "ok", failed: "critical" };

function DeliveryCell({ n }: { n: Notice }) {
  const t = useAppT();
  return (
    <div className="flex flex-col gap-1 text-xs">
      <StatusBadge tone={DELIVERY_TONE[n.state]} data-testid="status-delivery" data-state={n.state}>
        {t(`console.status.delivery.${n.state}`)}
      </StatusBadge>
      {n.state === "delivered" && (
        <span>
          {t("console.status.authority_ref")}: <span className={`font-mono ${TABLE_NOWRAP}`}>{n.authority_ref ?? t("console.none")}</span>,{" "}
          {t("console.status.submitted")} <Utc iso={n.submitted_at} className={TABLE_NOWRAP} />
        </span>
      )}
      {n.state === "pending" && (
        <span>
          {t("console.status.attempts", { n: n.attempts })}
          {n.next_at !== undefined && n.next_at !== null && (
            <>
              , {t("console.status.next_try")} <Utc iso={n.next_at} className={TABLE_NOWRAP} />
            </>
          )}
        </span>
      )}
      {n.state === "failed" && (
        <span>
          {t("console.status.failed_at")} <Utc iso={n.failed_at} className={TABLE_NOWRAP} />, {t("console.status.attempts", { n: n.attempts })}
        </span>
      )}
      {n.last_error !== null && (
        <span data-testid="status-last-error">
          {t("console.status.last_error")}: {n.last_error}
        </span>
      )}
    </div>
  );
}

function Action({ kind, certificate, onDone }: { kind: Kind; certificate: string; onDone(n: Notice, created: boolean): void }) {
  const t = useAppT();
  const api = useConsoleApi();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<unknown>(null);
  const send = () => {
    setBusy(true);
    setError(null);
    api
      .POST("/v1/admin/status", { body: { kind } })
      .then(({ data, response }) => {
        if (data !== undefined) onDone(data, response.status === 201);
      })
      .catch(setError)
      .finally(() => setBusy(false));
  };
  return (
    <div className="flex flex-col gap-2" data-testid="status-actions">
      <AlertDialog>
        <AlertDialogTrigger asChild>
          <Button disabled={busy} data-testid="status-action" data-kind={kind} className="self-start">
            {t(`console.status.action.${kind}`)}
          </Button>
        </AlertDialogTrigger>
        <AlertDialogContent data-testid="status-confirm">
          <AlertDialogHeader>
            <AlertDialogTitle>{t(`console.status.confirm.${kind}.title`)}</AlertDialogTitle>
            <AlertDialogDescription>{t(`console.status.confirm.${kind}.body`, { certificate })}</AlertDialogDescription>
          </AlertDialogHeader>
          <AlertDialogFooter>
            <AlertDialogCancel>{t("console.status.cancel")}</AlertDialogCancel>
            <AlertDialogAction onClick={send}>{t(`console.status.action.${kind}`)}</AlertDialogAction>
          </AlertDialogFooter>
        </AlertDialogContent>
      </AlertDialog>
      <ProblemNotice error={error} testId="status-problem" />
    </div>
  );
}

export function ConsoleStatusPage() {
  const t = useAppT();
  const api = useConsoleApi();
  const read = useCallback(() => api.GET("/v1/admin/status").then(body), [api]);
  const { data, error, failingSinceMs, reload } = usePoll(read, STATUS_PERIOD_MS);
  const [answer, setAnswer] = useState<{ n: Notice; created: boolean } | null>(null);
  const last = data === null ? undefined : standing(data.notices);
  const state = stateOf(last);
  return (
    <PageFrame id="status-heading" title={t("console.status.heading")} description={t("console.status.note")}>
      {failingSinceMs !== null && (
        <p role="alert" className="m-0 font-semibold">
          {t("console.unread_since")} <Utc iso={new Date(failingSinceMs).toISOString()} />
        </p>
      )}
      <ProblemNotice error={error} />
      {data !== null && (
        <>
          <section aria-labelledby="status-state-heading" className="flex flex-col gap-3 rounded border border-[var(--us-border)] p-3">
            <h2 id="status-state-heading" className="m-0 text-base font-semibold">
              {t("console.status.state")}
            </h2>
            <dl className="m-0 grid grid-cols-1 gap-2 md:grid-cols-3">
              <Fact term={t("console.status.state")}>
                <StatusBadge tone={STATE_TONE[state]} data-testid="status-state" data-state={state}>
                  {t(`console.status.state.${state}`)}
                </StatusBadge>
              </Fact>
              <Fact term={t("console.status.certificate")} testId="status-certificate">
                {data.certificate_id === null ? t("console.status.no_certificate_short") : <span className="break-all font-mono text-sm">{data.certificate_id}</span>}
              </Fact>
              <Fact term={t("console.status.last_notice")}>
                {last === undefined ? (
                  t("console.none")
                ) : (
                  <span className="text-sm">
                    {t(`console.status.kind.${last.kind}`)}, <Utc iso={last.at} />: {t(`console.status.delivery.${last.state}`)}
                  </span>
                )}
              </Fact>
            </dl>
            {data.certificate_id === null ? (
              <p role="status" className="m-0 text-sm" data-testid="status-no-certificate">
                {t("console.status.no_certificate")}
              </p>
            ) : (
              <RequireRole
                anyOf={["admin"]}
                fallback={
                  <p className="m-0 text-sm" data-testid="status-read-only">
                    {t("console.status.read_only")}
                  </p>
                }
              >
                <Action
                  key={NEXT[state]}
                  kind={NEXT[state]}
                  certificate={data.certificate_id}
                  onDone={(n, created) => {
                    setAnswer({ n, created });
                    reload();
                  }}
                />
              </RequireRole>
            )}
            {answer !== null && (
              <p role="status" className="m-0 text-sm" data-testid="status-answer" data-created={String(answer.created)}>
                {answer.created ? t("console.status.stored", { kind: t(`console.status.kind.${answer.n.kind}`) }) : t("console.status.already", { kind: t(`console.status.kind.${answer.n.kind}`) })}
              </p>
            )}
          </section>
          <section aria-labelledby="status-notices-heading" className="flex flex-col gap-2">
            <h2 id="status-notices-heading" className="m-0 text-base font-semibold">
              {t("console.status.notices")}
            </h2>
            {data.notices.length === 0 ? (
              <p className="m-0 text-sm" data-testid="status-no-notices">
                {t("console.status.no_notices")}
              </p>
            ) : (
              <Table data-testid="status-notices">
                <TableHeader>
                  <TableRow>
                    <TableHead>{t("console.status.col.kind")}</TableHead>
                    <TableHead>{t("console.status.col.at")}</TableHead>
                    <TableHead>{t("console.status.col.delivery")}</TableHead>
                    <TableHead>{t("console.status.col.requested_by")}</TableHead>
                    <TableHead>{t("console.status.col.reference")}</TableHead>
                    <TableHead>{t("console.status.col.created")}</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {data.notices.map((n) => (
                    <TableRow key={n.reference} data-testid="status-notice" data-kind={n.kind} data-state={n.state}>
                      <TableCell>{t(`console.status.kind.${n.kind}`)}</TableCell>
                      <TableCell>
                        <Utc iso={n.at} className={TABLE_NOWRAP} />
                      </TableCell>
                      <TableCell>
                        <DeliveryCell n={n} />
                      </TableCell>
                      <TableCell className="font-mono text-xs">{n.requested_by}</TableCell>
                      <TableCell className="break-all font-mono text-xs">{n.reference}</TableCell>
                      <TableCell>
                        <Utc iso={n.created_at} className={TABLE_NOWRAP} />
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            )}
          </section>
        </>
      )}
    </PageFrame>
  );
}
