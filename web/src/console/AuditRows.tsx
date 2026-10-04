"use client";

// The audit rows of one console entity (GET /v1/admin/events; spec 06
// T7): when, who (actor type and id), what, and the payload as stored,
// the reason among it. Read when shown; refreshed with the page.
import { useCallback } from "react";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@rootxkit/uspace-ui/ui";
import type { paths } from "@/api/generated/openapi";
import { ProblemNotice } from "@/components/ProblemNotice";
import { useAppT } from "@/i18n/t";
import { useConsoleApi } from "@/lib/consoleApi";
import { Utc } from "@/portal/common";
import { body, usePoll } from "./usePoll";

/** The console entities whose rows the API serves (its enum). */
export type EntityType = paths["/v1/admin/events"]["get"]["parameters"]["query"]["entity_type"];

/** How often the shown rows are read again. Display-only. */
const AUDIT_PERIOD_MS = 5000;

export function AuditRows({ entityType, entityId }: { entityType: EntityType; entityId: string }) {
  const t = useAppT();
  const api = useConsoleApi();
  const read = useCallback(
    () => api.GET("/v1/admin/events", { params: { query: { entity_type: entityType, entity_id: entityId } } }).then(body),
    [api, entityType, entityId],
  );
  const { data, error } = usePoll(read, AUDIT_PERIOD_MS);
  return (
    <div data-testid="audit" data-entity={`${entityType}:${entityId}`} className="flex flex-col gap-1">
      <ProblemNotice error={error} testId="audit-problem" />
      {data !== null && data.events.length === 0 && <p className="m-0">{t("console.audit.none")}</p>}
      {data !== null && data.events.length > 0 && (
        <Table>
          <TableHeader>
            <TableRow>
              <TableHead>{t("console.audit.at")}</TableHead>
              <TableHead>{t("console.audit.actor")}</TableHead>
              <TableHead>{t("console.audit.event")}</TableHead>
              <TableHead>{t("console.audit.payload")}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {data.events.map((e) => (
              <TableRow key={e.id} data-testid="audit-row" data-event={e.event_type} data-actor={e.actor_id}>
                <TableCell className="text-xs">
                  <Utc iso={e.ts} />
                </TableCell>
                <TableCell className="text-xs">
                  {e.actor_type}: <span className="font-mono">{e.actor_id}</span>
                </TableCell>
                <TableCell className="text-xs">{e.event_type}</TableCell>
                <TableCell className="max-w-md break-all font-mono text-xs">{JSON.stringify(e.payload)}</TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
      {data?.truncated === true && <p className="m-0">{t("portal.list.truncated")}</p>}
    </div>
  );
}
