"use client";

// The policy page (brief WP-18; LESSONS INV-03): the version in force
// with every value by its name (the unit is in the name), the values
// whose defaults are pending GCAA marked so, the newest versions with
// who stored each, when, why and what changed; for an admin a change
// made on the version in force: the values edited, the changes listed
// before they are sent, and a reason. The API validates, versions,
// audits and projects it, or refuses it (409 when the policy changed
// meanwhile, 503 when the KV cannot take it, nothing stored). The page
// compares only what the person typed with what the API sent, to show
// the change; it judges nothing.
import { useCallback, useMemo, useState } from "react";
import { RequireRole } from "@rootxkit/uspace-ui/auth/client";
import { Badge, Button, Input, Label, Table, TableBody, TableCell, TableHead, TableHeader, TableRow, Textarea } from "@rootxkit/uspace-ui/ui";
import type { components } from "@/api/generated/openapi";
import { ProblemNotice } from "@/components/ProblemNotice";
import { useAppT } from "@/i18n/t";
import { useConsoleApi } from "@/lib/consoleApi";
import { PageHeading, Utc } from "@/portal/common";
import { body, usePoll } from "./usePoll";

type Version = components["schemas"]["PolicyVersion"];
const POLICY_PERIOD_MS = 10000;

/** A value as the person edits it: numbers and strings as typed, lists comma-separated. */
function shown(v: unknown): string {
  return Array.isArray(v) ? v.join(", ") : String(v);
}

/** The typed text as the value's own JSON type (that of the version in force); null when it is not one. */
function parsed(text: string, like: unknown): unknown {
  if (Array.isArray(like)) {
    return text
      .split(",")
      .map((s) => s.trim())
      .filter((s) => s !== "");
  }
  if (typeof like === "number") {
    const n = Number(text.trim());
    return text.trim() === "" || !Number.isFinite(n) ? null : n;
  }
  return text;
}

function same(a: unknown, b: unknown): boolean {
  return JSON.stringify(a) === JSON.stringify(b);
}

function Changes({ changes, testId }: { changes: Version["changes"]; testId: string }) {
  const t = useAppT();
  if (changes.length === 0) return <p className="m-0 text-xs">{t("console.policy.no_changes")}</p>;
  return (
    <ul className="m-0 pl-5 text-xs" data-testid={testId}>
      {changes.map((c) => (
        <li key={c.field} data-field={c.field}>
          <span className="font-mono">{c.field}</span>: {JSON.stringify(c.from)} → {JSON.stringify(c.to)}
        </li>
      ))}
    </ul>
  );
}

function Editor({ current, pending, onStored }: { current: Version; pending: readonly string[]; onStored(v: Version | null): void }) {
  const t = useAppT();
  const api = useConsoleApi();
  const [edits, setEdits] = useState<Record<string, string>>({});
  const [reason, setReason] = useState("");
  const [error, setError] = useState<unknown>(null);
  const names = useMemo(() => Object.keys(current.values).sort(), [current]);
  const changed = useMemo(() => {
    const out: Record<string, unknown> = {};
    for (const [k, text] of Object.entries(edits)) {
      const v = parsed(text, current.values[k]);
      if (v !== null && !same(v, current.values[k])) out[k] = v;
    }
    return out;
  }, [edits, current]);
  const invalid = Object.entries(edits).filter(([k, text]) => parsed(text, current.values[k]) === null).map(([k]) => k);
  return (
    <section aria-labelledby="policy-edit-heading" className="flex flex-col gap-2" data-testid="policy-editor">
      <h2 id="policy-edit-heading" className="m-0 text-base font-semibold">
        {t("console.policy.edit_heading", { version: current.version })}
      </h2>
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>{t("console.policy.name")}</TableHead>
            <TableHead>{t("console.policy.value")}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {names.map((k) => {
            const id = `policy-${k}`;
            return (
              <TableRow key={k}>
                <TableCell className="font-mono text-xs">
                  <Label htmlFor={id}>{k}</Label>
                  {pending.includes(k) && (
                    <Badge variant="outline" className="ml-2">
                      {t("console.policy.pending_gcaa")}
                    </Badge>
                  )}
                </TableCell>
                <TableCell>
                  <Input id={id} value={edits[k] ?? shown(current.values[k])} onChange={(e) => setEdits((m) => ({ ...m, [k]: e.target.value }))} />
                </TableCell>
              </TableRow>
            );
          })}
        </TableBody>
      </Table>
      {invalid.length > 0 && (
        <p role="alert" className="m-0 text-sm">
          {t("console.policy.invalid", { names: invalid.join(", ") })}
        </p>
      )}
      <div data-testid="policy-preview">
        <h3 className="m-0 text-sm font-semibold">{t("console.policy.preview")}</h3>
        <Changes changes={Object.entries(changed).map(([field, to]) => ({ field, from: current.values[field], to }))} testId="policy-preview-changes" />
      </div>
      <Label htmlFor="policy-reason">{t("console.reason")}</Label>
      <Textarea id="policy-reason" value={reason} maxLength={500} onChange={(e) => setReason(e.target.value)} />
      <Button
        disabled={reason === "" || Object.keys(changed).length === 0 || invalid.length > 0}
        onClick={() => {
          setError(null);
          api
            .PUT("/v1/admin/policy", { body: { base_version: current.version, reason, values: changed } })
            .then(({ data }) => {
              setEdits({});
              setReason("");
              onStored(data ?? null);
            })
            .catch(setError);
        }}
      >
        {t("console.policy.store")}
      </Button>
      <ProblemNotice error={error} testId="policy-problem" />
    </section>
  );
}

export function ConsolePolicyPage() {
  const t = useAppT();
  const api = useConsoleApi();
  const read = useCallback(() => api.GET("/v1/admin/policy").then(body), [api]);
  const { data, error, reload } = usePoll(read, POLICY_PERIOD_MS);
  // Kept here: the editor starts afresh on the version it stored.
  const [stored, setStored] = useState<Version | null>(null);
  return (
    <section aria-labelledby="policy-heading" className="flex flex-col gap-3">
      <PageHeading id="policy-heading">{t("console.policy.heading")}</PageHeading>
      <p className="m-0 text-sm text-[var(--us-text-muted)]">{t("console.policy.note")}</p>
      <ProblemNotice error={error} />
      {data !== null && (
        <>
          <p className="m-0" data-testid="policy-current" data-version={data.current.version}>
            {data.current.version === 0 ? t("console.policy.defaults") : t("console.policy.current", { version: data.current.version, actor: data.current.actor ?? "" })}{" "}
            {data.current.created_at !== undefined && <Utc iso={data.current.created_at} />}
          </p>
          <RequireRole
            anyOf={["admin"]}
            fallback={
              <Table data-testid="policy-values">
                <TableBody>
                  {Object.keys(data.current.values)
                    .sort()
                    .map((k) => (
                      <TableRow key={k}>
                        <TableCell className="font-mono text-xs">
                          {k}
                          {data.pending_gcaa.includes(k) && (
                            <Badge variant="outline" className="ml-2">
                              {t("console.policy.pending_gcaa")}
                            </Badge>
                          )}
                        </TableCell>
                        <TableCell className="text-xs">{shown(data.current.values[k])}</TableCell>
                      </TableRow>
                    ))}
                </TableBody>
              </Table>
            }
          >
            <Editor
              key={data.current.version}
              current={data.current}
              pending={data.pending_gcaa}
              onStored={(v) => {
                setStored(v);
                reload();
              }}
            />
          </RequireRole>
          {stored !== null && (
            <p role="status" className="m-0 text-sm" data-testid="policy-stored">
              {t("console.policy.stored", { version: stored.version })}
            </p>
          )}
          <section aria-labelledby="policy-history-heading">
            <h2 id="policy-history-heading" className="m-0 mb-1 text-base font-semibold">
              {t("console.policy.history")}
            </h2>
            {data.history.length === 0 ? (
              <p className="m-0 text-sm">{t("console.policy.no_history")}</p>
            ) : (
              <ol className="m-0 flex list-none flex-col gap-2 p-0" data-testid="policy-history">
                {data.history.map((v) => (
                  <li key={v.version} data-testid="policy-version" data-version={v.version} className="rounded border border-[var(--us-border)] p-2 text-sm">
                    <p className="m-0 font-semibold">
                      {t("console.policy.version", { version: v.version, actor: v.actor ?? "" })} {v.created_at !== undefined && <Utc iso={v.created_at} />}
                    </p>
                    <p className="m-0">{v.reason}</p>
                    <Changes changes={v.changes} testId="policy-changes" />
                  </li>
                ))}
              </ol>
            )}
          </section>
        </>
      )}
    </section>
  );
}
