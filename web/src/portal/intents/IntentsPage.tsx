"use client";

// The operator's intents, newest first (GET /v1/intents, at most 500 as
// the API bounds it, and said when the API cut the list), with their
// decision, state, authorisation number and window; a state filter the
// API applies.
import { useEffect, useState } from "react";
import Link from "next/link";
import { RequireRole } from "@rootxkit/uspace-ui/auth/client";
import { Button, TABLE_NOWRAP, Table, TableBody, TableCell, TableHead, TableHeader, TableRow, cn } from "@rootxkit/uspace-ui/ui";
import { ProblemNotice } from "@/components/ProblemNotice";
import { useAppT } from "@/i18n/t";
import { useApi, type Schemas } from "@/lib/api";
import { PageFrame } from "@/components/frame";
import { RequireSession, Utc } from "../common";

const STATES = ["pending_validation", "pending_dss", "pending_authority", "accepted", "activated", "nonconforming", "contingent", "ended", "rejected", "withdrawn"] as const;

export function IntentsPage() {
  const t = useAppT();
  const api = useApi();
  const [state, setState] = useState<string>("");
  const [list, setList] = useState<{ intents: Omit<Schemas["IntentDecision"], "alternative">[]; truncated: boolean } | null>(null);
  const [error, setError] = useState<unknown>(null);
  useEffect(() => {
    let live = true;
    api
      .GET("/v1/intents", { params: { query: state === "" ? {} : { state } } })
      .then(({ data }) => {
        if (!live) return;
        setList(data ?? null);
        setError(null);
      })
      .catch((e: unknown) => live && setError(e));
    return () => {
      live = false;
    };
  }, [api, state]);
  return (
    <PageFrame id="intents-heading" title={t("portal.intents.heading")}>
      <RequireSession>
        <div className="flex flex-wrap items-end gap-3">
          <label className="flex flex-col gap-1 text-sm">
            {t("portal.intents.filter_state")}
            <select className="h-9 rounded-md border border-[var(--us-border)] bg-transparent px-2" value={state} onChange={(e) => setState(e.target.value)}>
              <option value="">{t("portal.intents.all_states")}</option>
              {STATES.map((s) => (
                <option key={s} value={s}>
                  {t(`portal.state.${s}`)}
                </option>
              ))}
            </select>
          </label>
          <RequireRole anyOf={["operator_admin", "remote_pilot"]}>
            <Button asChild>
              <Link href="/intents/new">{t("portal.nav.new_intent")}</Link>
            </Button>
          </RequireRole>
        </div>
        <ProblemNotice error={error} />
        {list?.truncated === true && (
          <p role="status" className="m-0 text-sm" data-testid="intents-truncated">
            {t("portal.list.truncated")}
          </p>
        )}
        {list !== null &&
          (list.intents.length === 0 ? (
            <p role="status" data-testid="no-intents">
              {t("portal.intents.none")}
            </p>
          ) : (
            <Table data-testid="intents">
              <TableHeader>
                <TableRow>
                  <TableHead className={TABLE_NOWRAP}>{t("portal.intents.intent")}</TableHead>
                  <TableHead>{t("portal.decision.state")}</TableHead>
                  <TableHead>{t("portal.intents.decision")}</TableHead>
                  <TableHead>{t("portal.decision.authorisation_number")}</TableHead>
                  <TableHead>{t("portal.decision.valid_from")}</TableHead>
                  <TableHead>{t("portal.decision.valid_to")}</TableHead>
                  <TableHead>{t("portal.decision.updated_at")}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {list.intents.map((d) => (
                  <TableRow key={d.intent_id} data-testid="intent-row">
                    <TableCell className={TABLE_NOWRAP}>
                      <Link href={`/intents/${d.intent_id}`} className="font-mono text-xs underline">
                        {d.intent_id}
                      </Link>
                    </TableCell>
                    <TableCell>{t(`portal.state.${d.state}`)}</TableCell>
                    <TableCell>{t(`portal.decision.decision.${d.decision}`)}</TableCell>
                    <TableCell className={cn("font-mono text-xs", TABLE_NOWRAP)}>{d.authorisation_number ?? ""}</TableCell>
                    <TableCell className={TABLE_NOWRAP}>
                      <Utc iso={d.valid_from} />
                    </TableCell>
                    <TableCell className={TABLE_NOWRAP}>
                      <Utc iso={d.valid_to} />
                    </TableCell>
                    <TableCell className={TABLE_NOWRAP}>
                      <Utc iso={d.updated_at} />
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          ))}
      </RequireSession>
    </PageFrame>
  );
}
