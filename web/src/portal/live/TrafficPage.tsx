"use client";

// Traffic information for one of the operator's intents (Art. 11; brief
// WP-17, S-M3): the live product of WS /v1/traffic on the map with the
// kit's symbology (trust class, age, identification status), the list of
// every track with what the product says of it (state, age_s, trust,
// source, altitude AMSL, speed, heading, identification status, own,
// peer unreachable), the degraded inputs with their time, the proximity
// alerts with their numbers and the other aircraft, their
// acknowledgement, and dropped_frames when there are any. The page shows
// and decides nothing: every age, state and number is the product's,
// and no string advises a manoeuvre (X-15).
import { useMemo, useState } from "react";
import { fmtHeading, fmtNum, fmtSpeed, useLang, useT } from "@rootxkit/uspace-ui/i18n";
import { TrackLayer } from "@rootxkit/uspace-ui/layers";
import { AgeLegend, IdentificationLegend, TrackLegend } from "@rootxkit/uspace-ui/legend";
import { useNowMs } from "@rootxkit/uspace-ui/live";
import type { Trust } from "@rootxkit/uspace-ui/model";
import { FeedStatusBar } from "@rootxkit/uspace-ui/status";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@rootxkit/uspace-ui/ui";
import { useAppT } from "@/i18n/t";
import { PageHeading, RequireSession, Utc } from "../common";
import { IntentPicker } from "../IntentPicker";
import { PortalMap } from "../PortalMap";
import { AlertsTable } from "./AlertsTable";
import { DegradedList, StreamFacts } from "./StreamFacts";
import { TRAIL_POINTS, useTraffic } from "./useStreams";

/** How often ages are redrawn. Display-only. */
const TICK_MS = 1000;

function Live({ intentId }: { intentId: string }) {
  const t = useAppT();
  const kitT = useT();
  const { lang } = useLang();
  const nowMs = useNowMs(TICK_MS);
  const traffic = useTraffic(intentId);
  const { feed, product, tracks, facts } = traffic;
  const [selected, setSelected] = useState<string | null>(null);
  const list = useMemo(() => [...tracks.values()].sort((a, b) => a.trackId.localeCompare(b.trackId)), [tracks]);
  const trustCounts = useMemo(() => {
    const c: Partial<Record<Trust, number>> = {};
    for (const tr of list) c[tr.trust] = (c[tr.trust] ?? 0) + 1;
    return c;
  }, [list]);
  // The product's active proximity alerts, and any alert frame the
  // stream sent since (newer state wins by id).
  const alerts = useMemo(() => {
    const m = new Map((product?.alerts ?? []).map((a) => [a.alertId, a]));
    for (const a of traffic.alerts.values()) m.set(a.alertId, a);
    return [...m.values()];
  }, [product, traffic.alerts]);
  return (
    <div className="flex flex-col gap-3">
      <section aria-label={t("portal.feed.label")} className="flex flex-wrap items-start gap-x-6 gap-y-1 rounded border border-[var(--us-border)] bg-[var(--us-surface-sunken)] px-3 py-2 text-xs" data-testid="feed" data-connection={feed.connection}>
        <FeedStatusBar status={feed} nowMs={nowMs} />
        <StreamFacts feed={feed} droppedFrames={product?.droppedFrames ?? null} counters={traffic.counters} at={product?.at ?? null} policyVersion={product?.policyVersion ?? null} cisVersion={product?.cisVersion ?? null} />
      </section>
      <DegradedList degraded={product?.degraded ?? null} feedDegraded={feed.degraded} />
      <div className="grid gap-3 lg:grid-cols-[1fr_20rem]">
        <PortalMap className="h-[420px] lg:h-[560px]">
          {feed.staleAfterS !== null && (
            <TrackLayer tracks={list} staleAfterS={feed.staleAfterS} nowMs={nowMs} selectedId={selected} onSelect={setSelected} trails={{ points: TRAIL_POINTS }} labels />
          )}
        </PortalMap>
        <div className="flex flex-col gap-2">
          {feed.staleAfterS === null && <p role="status">{t("portal.traffic.no_threshold")}</p>}
          <TrackLegend counts={trustCounts} />
          <IdentificationLegend />
          {feed.staleAfterS !== null && <AgeLegend staleAfterS={feed.staleAfterS} />}
        </div>
      </div>
      <section aria-labelledby="prox-heading">
        <h2 id="prox-heading" className="m-0 mb-1 text-base font-semibold">
          {t("portal.traffic.alerts_heading")}
        </h2>
        <AlertsTable alerts={alerts} testId="traffic-alerts" />
      </section>
      <section aria-labelledby="tracks-heading">
        <h2 id="tracks-heading" className="m-0 mb-1 text-base font-semibold">
          {t("portal.traffic.tracks_heading", { n: list.length })}
        </h2>
        {product === null ? (
          <p role="status" data-testid="no-product">
            {t("portal.traffic.waiting")}
          </p>
        ) : list.length === 0 ? (
          <p role="status" data-testid="no-tracks">
            {t("portal.traffic.no_tracks", { at: product.at })}
          </p>
        ) : (
          <Table data-testid="tracks">
            <TableHeader>
              <TableRow>
                <TableHead>{t("portal.traffic.track")}</TableHead>
                <TableHead>{t("portal.traffic.trust")}</TableHead>
                <TableHead>{t("portal.traffic.state")}</TableHead>
                <TableHead>{t("portal.traffic.age")}</TableHead>
                <TableHead>{t("portal.traffic.alt_amsl")}</TableHead>
                <TableHead>{t("portal.traffic.speed")}</TableHead>
                <TableHead>{t("portal.traffic.course")}</TableHead>
                <TableHead>{t("portal.traffic.ident")}</TableHead>
                <TableHead>{t("portal.traffic.report")}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {list.map((tr) => {
                const f = facts.get(tr.trackId);
                return (
                  <TableRow key={tr.trackId} data-testid="track-row" data-track-id={tr.trackId} data-own={f?.own === true ? "true" : "false"} aria-selected={selected === tr.trackId}>
                    <TableCell className="font-mono text-xs">
                      {tr.trackId}
                      {f?.own === true && <span className="ml-1 font-sans font-semibold">{t("portal.traffic.own")}</span>}
                      {f?.callsign !== null && f?.callsign !== undefined && <span className="ml-1 font-sans">{f.callsign}</span>}
                    </TableCell>
                    <TableCell>
                      {kitT(`trust.${tr.trust}`)} · {tr.source}
                    </TableCell>
                    <TableCell>
                      {f === undefined ? "" : t(`portal.traffic.track_state.${f.state}`)}
                      {f?.peerUnavailable === true && <p className="m-0 text-xs">{t("portal.traffic.peer_unavailable")}</p>}
                    </TableCell>
                    <TableCell>{f === undefined ? "" : fmtNum(f.ageS, 1, "s", lang)}</TableCell>
                    <TableCell>{fmtNum(tr.altAmslM, 0, "m", lang)}</TableCell>
                    <TableCell>{fmtSpeed(tr.speedMs, lang)}</TableCell>
                    <TableCell>{fmtHeading(tr.trackDeg)}</TableCell>
                    <TableCell>{kitT(`ident.status.${f?.ident ?? "none"}`)}</TableCell>
                    <TableCell className="text-xs">
                      <Utc iso={tr.times.capturedAt} />
                      {tr.emergency && <p className="m-0 font-semibold">{t("portal.traffic.emergency")}</p>}
                    </TableCell>
                  </TableRow>
                );
              })}
            </TableBody>
          </Table>
        )}
      </section>
    </div>
  );
}

export function TrafficPage({ intentId }: { intentId: string | null }) {
  const t = useAppT();
  return (
    <section aria-labelledby="traffic-heading" className="flex flex-col gap-3">
      <PageHeading id="traffic-heading">{t("portal.traffic.heading")}</PageHeading>
      <p className="m-0 text-sm text-[var(--us-text-muted)]">{t("portal.traffic.note")}</p>
      <RequireSession>
        <IntentPicker path="/traffic" current={intentId} />
        {intentId !== null && <Live key={intentId} intentId={intentId} />}
      </RequireSession>
    </section>
  );
}
