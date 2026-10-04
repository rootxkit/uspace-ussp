"use client";

// The alerts of one of the operator's intents (Art. 13; brief WP-17):
// WS /v1/alerts sends the active ones on connect, every alert/v1 of the
// intent's flight as it comes, and repeats an unacknowledged critical
// alert every escalation_repeat_s until it is acknowledged or cleared.
// The list shows each with its numbers and the other aircraft, how many
// times it arrived (the repeats) and its escalation, and an
// operator_admin or a remote_pilot acknowledges it.
import { useNowMs } from "@rootxkit/uspace-ui/live";
import { FeedStatusBar } from "@rootxkit/uspace-ui/status";
import { useAppT } from "@/i18n/t";
import { PageHeading, RequireSession } from "../common";
import { IntentPicker } from "../IntentPicker";
import { AlertsTable } from "./AlertsTable";
import { DegradedList, StreamFacts } from "./StreamFacts";
import { useAlertStream } from "./useStreams";

function Stream({ intentId }: { intentId: string }) {
  const t = useAppT();
  const nowMs = useNowMs(1000);
  const s = useAlertStream(intentId);
  return (
    <div className="flex flex-col gap-3">
      <section aria-label={t("portal.feed.label")} className="flex flex-wrap items-start gap-x-6 gap-y-1 rounded border border-[var(--us-border)] bg-[var(--us-surface-sunken)] px-3 py-2 text-xs" data-testid="feed" data-connection={s.feed.connection}>
        <FeedStatusBar status={s.feed} nowMs={nowMs} />
        <StreamFacts feed={s.feed} droppedFrames={null} counters={s.counters} />
      </section>
      <DegradedList degraded={null} feedDegraded={s.feed.degraded} />
      <AlertsTable alerts={[...s.alerts.values()]} testId="stream-alerts" />
    </div>
  );
}

export function AlertsPage({ intentId }: { intentId: string | null }) {
  const t = useAppT();
  return (
    <section aria-labelledby="alerts-heading" className="flex flex-col gap-3">
      <PageHeading id="alerts-heading">{t("portal.alerts.heading")}</PageHeading>
      <p className="m-0 text-sm text-[var(--us-text-muted)]">{t("portal.alerts.note")}</p>
      <RequireSession>
        <IntentPicker path="/alerts" current={intentId} />
        {intentId !== null && <Stream key={intentId} intentId={intentId} />}
      </RequireSession>
    </section>
  );
}
