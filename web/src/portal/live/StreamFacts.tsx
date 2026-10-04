"use client";

// What a stream says about itself, in words (LESSONS B-11, SC-22: an
// empty picture must never look like an empty sky): the product's time,
// policy and CIS versions, dropped_frames when there are any, what this
// page refused and did not show; and every degraded input with the time
// since which it is degraded and why, as the product names it.
import { degradedLabel } from "@rootxkit/uspace-ui/status";
import { useT } from "@rootxkit/uspace-ui/i18n";
import type { LiveFeed } from "@rootxkit/uspace-ui/live";
import { useAppT } from "@/i18n/t";
import { Utc } from "../common";
import type { Degraded } from "./adapt";
import type { StreamCounters } from "./useStreams";

export function StreamFacts({
  feed,
  droppedFrames,
  counters,
  at,
  policyVersion,
  cisVersion,
}: {
  feed: LiveFeed;
  droppedFrames: number | null;
  counters: StreamCounters;
  at?: string | null;
  policyVersion?: number | null;
  cisVersion?: string | null;
}) {
  const t = useAppT();
  const dropped = Math.max(droppedFrames ?? 0, feed.droppedFrames);
  const refused = counters.productsRefused + counters.tracksRefused + counters.alertsRefused;
  return (
    <dl className="m-0 flex flex-wrap gap-x-4 gap-y-1">
      {at !== undefined && (
        <div className="flex gap-1">
          <dt>{t("portal.feed.product_at")}</dt>
          <dd className="m-0">
            <Utc iso={at} />
          </dd>
        </div>
      )}
      <div className="flex gap-1">
        <dt>{t("portal.feed.policy")}</dt>
        <dd className="m-0">{policyVersion ?? feed.policyVersion ?? t("portal.feed.unknown")}</dd>
      </div>
      {cisVersion !== undefined && (
        <div className="flex gap-1">
          <dt>{t("portal.feed.cis")}</dt>
          <dd className="m-0">{cisVersion ?? t("portal.feed.unknown")}</dd>
        </div>
      )}
      {dropped > 0 && (
        <div className="flex gap-1 font-semibold" data-testid="dropped-frames">
          <dt>{t("portal.feed.dropped")}</dt>
          <dd className="m-0">{dropped}</dd>
        </div>
      )}
      {refused > 0 && (
        <div className="flex gap-1" data-testid="refused">
          <dt>{t("portal.feed.refused")}</dt>
          <dd className="m-0">{refused}</dd>
        </div>
      )}
      {counters.framesNotShown > 0 && (
        <div className="flex gap-1">
          <dt>{t("portal.feed.not_shown")}</dt>
          <dd className="m-0">{counters.framesNotShown}</dd>
        </div>
      )}
      {counters.alertsEvicted > 0 && (
        <div className="flex gap-1">
          <dt>{t("portal.feed.alerts_evicted")}</dt>
          <dd className="m-0">{counters.alertsEvicted}</dd>
        </div>
      )}
    </dl>
  );
}

/** The degraded inputs: the product's with their time and reason, else the status frame's slugs. */
export function DegradedList({ degraded, feedDegraded }: { degraded: readonly Degraded[] | null; feedDegraded: readonly string[] }) {
  const t = useAppT();
  const kitT = useT();
  const items: readonly Degraded[] = degraded ?? feedDegraded.map((s) => ({ input: s, since: null, reason: "" }));
  if (items.length === 0) return null;
  return (
    <section role="alert" aria-labelledby="degraded-heading" data-testid="degraded" className="rounded border-2 border-[var(--us-severity-warning)] p-3 text-sm">
      <h2 id="degraded-heading" className="m-0 text-base font-semibold">
        {t("portal.degraded.heading")}
      </h2>
      <ul className="m-0 mt-1 flex flex-col gap-1 pl-5">
        {items.map((d) => (
          <li key={d.input} data-testid={`degraded-${d.input}`}>
            <span className="font-semibold">{degradedLabel(d.input, kitT)}</span>{" "}
            {d.since === null ? t("portal.degraded.never_up") : (
              <>
                {t("portal.degraded.since")} <Utc iso={d.since} />
              </>
            )}
            {d.reason !== "" && <span>: {d.reason}</span>}
          </li>
        ))}
      </ul>
    </section>
  );
}
