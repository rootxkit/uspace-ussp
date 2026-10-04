"use client";

// The emergency workflow (spec 01 §3 S11; brief WP-18): a communication
// checklist with timestamps for one flight, never a control panel. A
// supervisor opens a case with a reason, adds timestamped notes (each may
// tick one step of the policy's emergency checklist) and closes it with
// an outcome; every step is an audit row, shown with the case. The case
// shows the intent's emergency contact reference and the authority's
// procedure for resolving it: the USSP keeps the reference only (no PII).
// Nothing here reaches an aircraft.
import Link from "next/link";
import { useCallback, useState } from "react";
import { ApiError } from "@rootxkit/uspace-ui/api";
import { RequireRole } from "@rootxkit/uspace-ui/auth/client";
import { Button, Input, Label, Table, TableBody, TableCell, TableHead, TableHeader, TableRow, Textarea } from "@rootxkit/uspace-ui/ui";
import type { components } from "@/api/generated/openapi";
import { ProblemNotice } from "@/components/ProblemNotice";
import { useAppT } from "@/i18n/t";
import { useConsoleApi } from "@/lib/consoleApi";
import { Fact, PageHeading, Utc } from "@/portal/common";
import { AuditRows } from "./AuditRows";
import { body, usePoll } from "./usePoll";

type Case = components["schemas"]["EmergencyCase"];
const CASES_PERIOD_MS = 5000;

function OpenForm({ flightId, onOpened }: { flightId: string | null; onOpened(flightId: string): void }) {
  const t = useAppT();
  const api = useConsoleApi();
  const [flight, setFlight] = useState(flightId ?? "");
  const [reason, setReason] = useState("");
  const [error, setError] = useState<unknown>(null);
  return (
    <RequireRole anyOf={["supervisor"]} fallback={<p className="m-0 text-sm">{t("console.read_only")}</p>}>
      <form
        className="flex max-w-xl flex-col gap-2"
        data-testid="case-open"
        onSubmit={(e) => {
          e.preventDefault();
          setError(null);
          api
            .POST("/v1/admin/emergency/{flight_id}", { params: { path: { flight_id: flight } }, body: { action: "open", reason } })
            .then(() => {
              setReason("");
              onOpened(flight);
            })
            .catch(setError);
        }}
      >
        <h2 className="m-0 text-base font-semibold">{t("console.emergency.open_heading")}</h2>
        {flightId === null && (
          <>
            <Label htmlFor="case-flight">{t("console.emergency.flight")}</Label>
            <Input id="case-flight" value={flight} onChange={(e) => setFlight(e.target.value.trim())} />
          </>
        )}
        <Label htmlFor="case-reason">{t("console.reason")}</Label>
        <Textarea id="case-reason" value={reason} maxLength={500} onChange={(e) => setReason(e.target.value)} />
        <Button type="submit" disabled={reason === "" || flight === ""}>
          {t("console.emergency.open")}
        </Button>
        <ProblemNotice error={error} testId="case-problem" />
      </form>
    </RequireRole>
  );
}

function CaseView({ c, onChanged }: { c: Case; onChanged(): void }) {
  const t = useAppT();
  const api = useConsoleApi();
  const [text, setText] = useState("");
  const [step, setStep] = useState("");
  const [outcome, setOutcome] = useState("");
  const [error, setError] = useState<unknown>(null);
  const post = (b: components["schemas"]["EmergencyAction"], done: () => void) => {
    setError(null);
    api
      .POST("/v1/admin/emergency/{flight_id}", { params: { path: { flight_id: c.flight_id } }, body: b })
      .then(() => {
        done();
        onChanged();
      })
      .catch(setError);
  };
  const open = c.closed_at === undefined;
  return (
    <article className="flex flex-col gap-3" data-testid="case" data-case-id={c.case_id} data-open={open ? "true" : "false"}>
      <dl className="m-0 grid grid-cols-1 gap-2 md:grid-cols-3">
        <Fact term={t("console.emergency.flight")}>
          <span className="font-mono">{c.flight_id}</span> · {c.uas_serial}
        </Fact>
        <Fact term={t("console.emergency.opened")}>
          {c.opened_by} <Utc iso={c.opened_at} />
        </Fact>
        <Fact term={t("console.reason")}>{c.reason}</Fact>
        <Fact term={t("console.emergency.contact_ref")} testId="contact-ref">
          {c.contact_ref ?? t("console.emergency.no_contact_ref")}
        </Fact>
        <Fact term={t("console.emergency.procedure")}>{c.contact_procedure}</Fact>
        <Fact term={t("console.emergency.record")}>
          <span className="font-mono">{c.record_link}</span>
        </Fact>
        {!open && (
          <Fact term={t("console.emergency.closed")} testId="case-closed">
            {c.closed_by} <Utc iso={c.closed_at} />: {c.outcome}
          </Fact>
        )}
      </dl>
      <section aria-labelledby="checklist-heading">
        <h2 id="checklist-heading" className="m-0 mb-1 text-base font-semibold">
          {t("console.emergency.checklist")}
        </h2>
        <ul className="m-0 flex list-none flex-col gap-1 p-0 text-sm" data-testid="checklist">
          {c.checklist.map((s) => (
            <li key={s.step} data-testid="checklist-step" data-step={s.step} data-done={s.done_at !== undefined ? "true" : "false"}>
              <span aria-hidden="true">{s.done_at !== undefined ? "☑" : "☐"}</span> <span className="font-mono">{s.step}</span>
              {s.done_at !== undefined && (
                <>
                  {" "}
                  {s.done_by} <Utc iso={s.done_at} />
                </>
              )}
            </li>
          ))}
        </ul>
      </section>
      <section aria-labelledby="notes-heading">
        <h2 id="notes-heading" className="m-0 mb-1 text-base font-semibold">
          {t("console.emergency.notes", { n: c.notes.length })}
        </h2>
        <ol className="m-0 flex flex-col gap-1 pl-5 text-sm" data-testid="notes">
          {c.notes.map((n, i) => (
            <li key={`${n.at}-${i}`} data-testid="note">
              <Utc iso={n.at} /> {n.author}
              {n.step !== undefined && <span className="font-mono"> [{n.step}]</span>}: {n.text}
            </li>
          ))}
        </ol>
      </section>
      {open && (
        <RequireRole anyOf={["supervisor"]} fallback={<p className="m-0 text-sm">{t("console.read_only")}</p>}>
          <div className="grid gap-4 md:grid-cols-2">
            <form
              className="flex flex-col gap-2"
              data-testid="case-note"
              onSubmit={(e) => {
                e.preventDefault();
                post({ action: "note", text, ...(step === "" ? {} : { step }) }, () => {
                  setText("");
                  setStep("");
                });
              }}
            >
              <Label htmlFor="note-text">{t("console.emergency.note")}</Label>
              <Textarea id="note-text" value={text} maxLength={2000} onChange={(e) => setText(e.target.value)} />
              <Label htmlFor="note-step">{t("console.emergency.step")}</Label>
              <select
                id="note-step"
                value={step}
                onChange={(e) => setStep(e.target.value)}
                className="h-9 rounded-md border border-[var(--us-border)] bg-[var(--us-surface)] px-2 text-sm"
              >
                <option value="">{t("console.emergency.no_step")}</option>
                {c.checklist.map((s) => (
                  <option key={s.step} value={s.step}>
                    {s.step}
                  </option>
                ))}
              </select>
              <Button type="submit" disabled={text === ""}>
                {t("console.emergency.add_note")}
              </Button>
            </form>
            <form
              className="flex flex-col gap-2"
              data-testid="case-close"
              onSubmit={(e) => {
                e.preventDefault();
                post({ action: "close", outcome }, () => setOutcome(""));
              }}
            >
              <Label htmlFor="case-outcome">{t("console.emergency.outcome")}</Label>
              <Textarea id="case-outcome" value={outcome} maxLength={500} onChange={(e) => setOutcome(e.target.value)} />
              <Button type="submit" variant="outline" disabled={outcome === ""}>
                {t("console.emergency.close")}
              </Button>
            </form>
          </div>
        </RequireRole>
      )}
      <ProblemNotice error={error} testId="case-problem" />
      <section aria-labelledby="case-audit-heading">
        <h2 id="case-audit-heading" className="m-0 mb-1 text-base font-semibold">
          {t("console.audit.heading")}
        </h2>
        <AuditRows entityType="emergency_case" entityId={c.case_id} />
      </section>
    </article>
  );
}

/** One flight's newest case, or the form that opens one. */
export function ConsoleCasePage({ flightId }: { flightId: string }) {
  const t = useAppT();
  const api = useConsoleApi();
  const read = useCallback(
    () =>
      api.GET("/v1/admin/emergency/{flight_id}", { params: { path: { flight_id: flightId } } }).then(
        (r) => r.data ?? null,
        (e: unknown) => {
          // No case yet is an answer here, not a failure.
          if (e instanceof ApiError && e.status === 404) return null;
          throw e;
        },
      ),
    [api, flightId],
  );
  const { data, error, okAtMs, reload } = usePoll(read, CASES_PERIOD_MS);
  return (
    <section aria-labelledby="case-heading" className="flex flex-col gap-3">
      <PageHeading id="case-heading">{t("console.emergency.case_heading")}</PageHeading>
      <p className="m-0 text-sm text-[var(--us-text-muted)]">{t("console.emergency.note_text")}</p>
      <Link href="/console/emergency" className="text-sm underline">
        {t("console.emergency.all")}
      </Link>
      <ProblemNotice error={error} />
      {okAtMs !== null && data === null && <OpenForm flightId={flightId} onOpened={reload} />}
      {data !== null && (
        <>
          <CaseView c={data} onChanged={reload} />
          {data.closed_at !== undefined && <OpenForm flightId={flightId} onOpened={reload} />}
        </>
      )}
    </section>
  );
}

/** Every open case and those closed in the last days. */
export function ConsoleEmergencyPage() {
  const t = useAppT();
  const api = useConsoleApi();
  const read = useCallback(() => api.GET("/v1/admin/emergency").then(body), [api]);
  const { data, error, reload } = usePoll(read, CASES_PERIOD_MS);
  return (
    <section aria-labelledby="emergency-heading" className="flex flex-col gap-3">
      <PageHeading id="emergency-heading">{t("console.emergency.heading")}</PageHeading>
      <p className="m-0 text-sm text-[var(--us-text-muted)]">{t("console.emergency.note_text")}</p>
      <ProblemNotice error={error} />
      {data !== null && data.cases.length === 0 && <p role="status">{t("console.emergency.none")}</p>}
      {data !== null && data.cases.length > 0 && (
        <Table data-testid="cases">
          <TableHeader>
            <TableRow>
              <TableHead>{t("console.emergency.flight")}</TableHead>
              <TableHead>{t("console.emergency.opened")}</TableHead>
              <TableHead>{t("console.reason")}</TableHead>
              <TableHead>{t("console.emergency.state")}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {data.cases.map((c) => (
              <TableRow key={c.case_id} data-testid="case-row" data-case-id={c.case_id} data-open={c.closed_at === undefined ? "true" : "false"}>
                <TableCell className="text-xs">
                  <Link href={`/console/emergency/${c.flight_id}`} className="font-mono underline">
                    {c.flight_id}
                  </Link>
                  <p className="m-0">{c.uas_serial}</p>
                </TableCell>
                <TableCell className="text-xs">
                  {c.opened_by} <Utc iso={c.opened_at} />
                </TableCell>
                <TableCell className="text-xs">{c.reason}</TableCell>
                <TableCell className="text-xs">
                  {c.closed_at === undefined ? (
                    t("console.emergency.open_state")
                  ) : (
                    <>
                      {t("console.emergency.closed")} <Utc iso={c.closed_at} />: {c.outcome}
                    </>
                  )}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
      {data?.truncated === true && <p className="m-0">{t("portal.list.truncated")}</p>}
      <OpenForm flightId={null} onOpened={reload} />
    </section>
  );
}
