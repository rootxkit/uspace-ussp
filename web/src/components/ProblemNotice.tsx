"use client";

// A refusal as the API wrote it (RFC 9457, M28): the status, the problem
// type's slug, the detail and every field error with its path (the
// kit's FieldErrors, with "truncated" when the API cut the list). An
// answer that never came (the network, the BFF) says so; nothing is
// summarised away.
import { ApiError } from "@rootxkit/uspace-ui/api";
import { FieldErrors } from "@rootxkit/uspace-ui/form";
import { useAppT } from "@/i18n/t";

export function ProblemNotice({ error, testId = "problem" }: { error: unknown; testId?: string }) {
  const t = useAppT();
  if (error === null || error === undefined) return null;
  if (!(error instanceof ApiError)) {
    return (
      <div role="alert" data-testid={testId} className="rounded border border-[var(--us-danger)] p-3 text-sm">
        {t("portal.problem.no_answer", { error: error instanceof Error ? error.message : String(error) })}
      </div>
    );
  }
  const p = error.problem;
  return (
    <div role="alert" data-testid={testId} data-slug={error.slug ?? ""} className="flex flex-col gap-1 rounded border border-[var(--us-danger)] p-3 text-sm">
      <p className="m-0 font-semibold">
        {t("portal.problem.heading", { status: error.status, slug: error.slug ?? t("portal.problem.no_slug") })}
      </p>
      {p?.detail ? <p className="m-0">{p.detail}</p> : null}
      {error.retryAfterS !== null && <p className="m-0">{t("portal.problem.retry_after", { s: error.retryAfterS })}</p>}
      {p !== null && p.errors.length > 0 && <FieldErrors errors={p.errors} truncated={p.truncated === true} />}
      {error.requestId !== null && <p className="m-0 text-xs text-[var(--us-text-muted)]">{t("portal.problem.request_id", { id: error.requestId })}</p>}
    </div>
  );
}
