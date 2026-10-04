"use client";

// The console's live map (brief WP-18): traffic-ws's stream with the
// staff session cookie on a same-origin upgrade (M22), subscribed by the
// map's viewport (console/subscribe/v1 {bbox, layers} on every settled
// move, M29); its traffic/product/v1 bodies on the map with the kit's
// symbology and in a list with each track's state and age (a switched-off
// source's tracks show source_disabled and then age out), the stream's
// degraded inputs with their time, and the escalated alerts the
// supervisor must handle. It shows and decides nothing.
import { useCallback, useMemo, useState } from "react";
import { fmtNum, useLang, useT } from "@rootxkit/uspace-ui/i18n";
import { TrackLayer } from "@rootxkit/uspace-ui/layers";
import { AgeLegend, IdentificationLegend, TrackLegend } from "@rootxkit/uspace-ui/legend";
import { createTrackStore, subscribeFrame, useFeed, useNowMs, useStore, type ConsoleFrame } from "@rootxkit/uspace-ui/live";
import type { BBox } from "@rootxkit/uspace-ui/map";
import type { Trust } from "@rootxkit/uspace-ui/model";
import { FeedStatusBar } from "@rootxkit/uspace-ui/status";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@rootxkit/uspace-ui/ui";
import { useRouter } from "next/navigation";
import { useRuntimeConfig } from "@/components/Providers";
import { useAppT } from "@/i18n/t";
import { PageHeading, Utc } from "@/portal/common";
import { PortalMap, ViewBox } from "@/portal/PortalMap";
import { PRODUCT_SCHEMA, adaptProduct, type Product, type TrackFacts } from "@/portal/live/adapt";
import { DegradedList, StreamFacts } from "@/portal/live/StreamFacts";
import { MAX_TRACKS, TRAIL_POINTS, type StreamCounters } from "@/portal/live/useStreams";
import { EscalationsPanel } from "./EscalationsPanel";
import { LOGIN } from "./ConsoleShell";

const TICK_MS = 1000;
const ZERO: StreamCounters = { productsRefused: 0, tracksRefused: 0, alertsRefused: 0, framesNotShown: 0, alertsEvicted: 0 };

export function ConsoleMapPage() {
  const t = useAppT();
  const kitT = useT();
  const { lang } = useLang();
  const router = useRouter();
  const cfg = useRuntimeConfig();
  const nowMs = useNowMs(TICK_MS);
  const [store] = useState(() => createTrackStore({ trailPoints: TRAIL_POINTS, maxTracks: MAX_TRACKS }));
  const [product, setProduct] = useState<Product | null>(null);
  const [facts, setFacts] = useState<ReadonlyMap<string, TrackFacts>>(new Map());
  const [counters, setCounters] = useState<StreamCounters>(ZERO);
  const onFrame = (f: ConsoleFrame) => {
    if (f.schema !== PRODUCT_SCHEMA) {
      // alert/v1 bodies and the snapshot: the alerts page and the
      // escalations read the record; the map draws the product.
      if (f.schema !== "alert/v1") setCounters((c) => ({ ...c, framesNotShown: c.framesNotShown + 1 }));
      return;
    }
    const p = adaptProduct(f);
    if (p === null) {
      setCounters((c) => ({ ...c, productsRefused: c.productsRefused + 1 }));
      return;
    }
    if (p.throttled) for (const tr of p.tracks) store.upsert(tr.view);
    else store.replace(p.tracks.map((tr) => tr.view));
    setFacts((prev) => {
      const next = p.throttled ? new Map(prev) : new Map<string, TrackFacts>();
      for (const tr of p.tracks) next.set(tr.view.trackId, tr.facts);
      return next;
    });
    if (p.tracksRefused > 0) setCounters((c) => ({ ...c, tracksRefused: c.tracksRefused + p.tracksRefused }));
    setProduct(p);
  };
  const feed = useFeed({ url: "/v1/traffic", onUnauthorized: () => router.replace(LOGIN), onFrame });
  const send = feed.send;
  const onBox = useCallback((b: BBox) => send(subscribeFrame([b.minLng, b.minLat, b.maxLng, b.maxLat], ["tracks", "manned", "alerts"])), [send]);
  const tracks = useStore(store);
  const list = useMemo(() => [...tracks.values()].sort((a, b) => a.trackId.localeCompare(b.trackId)), [tracks]);
  const trustCounts = useMemo(() => {
    const c: Partial<Record<Trust, number>> = {};
    for (const tr of list) c[tr.trust] = (c[tr.trust] ?? 0) + 1;
    return c;
  }, [list]);
  const [selected, setSelected] = useState<string | null>(null);
  return (
    <section aria-labelledby="map-heading" className="flex flex-col gap-3">
      <PageHeading id="map-heading">{t("console.map.heading")}</PageHeading>
      <p className="m-0 text-sm text-[var(--us-text-muted)]">{t("console.map.note")}</p>
      <section
        aria-label={t("portal.feed.label")}
        className="flex flex-wrap items-start gap-x-6 gap-y-1 rounded border border-[var(--us-border)] bg-[var(--us-surface-sunken)] px-3 py-2 text-xs"
        data-testid="console-feed"
        data-connection={feed.connection}
      >
        <FeedStatusBar status={feed} nowMs={nowMs} />
        <StreamFacts feed={feed} droppedFrames={product?.droppedFrames ?? null} counters={counters} at={product?.at ?? null} policyVersion={product?.policyVersion ?? null} cisVersion={product?.cisVersion ?? null} />
      </section>
      <DegradedList degraded={product?.degraded ?? null} feedDegraded={feed.degraded} />
      <EscalationsPanel />
      <div className="grid gap-3 lg:grid-cols-[1fr_20rem]">
        <PortalMap className="h-[420px] lg:h-[520px]">
          <ViewBox margin={cfg.geoMarginFraction} onChange={onBox} />
          {feed.staleAfterS !== null && (
            <TrackLayer tracks={list} staleAfterS={feed.staleAfterS} nowMs={nowMs} selectedId={selected} onSelect={setSelected} trails={{ points: TRAIL_POINTS }} labels />
          )}
        </PortalMap>
        <div className="flex flex-col gap-2">
          <TrackLegend counts={trustCounts} />
          <IdentificationLegend />
          {feed.staleAfterS !== null && <AgeLegend staleAfterS={feed.staleAfterS} />}
        </div>
      </div>
      <section aria-labelledby="console-tracks-heading">
        <h2 id="console-tracks-heading" className="m-0 mb-1 text-base font-semibold">
          {t("portal.traffic.tracks_heading", { n: list.length })}
        </h2>
        {product === null ? (
          <p role="status" data-testid="console-no-product">
            {t("portal.traffic.waiting")}
          </p>
        ) : list.length === 0 ? (
          <p role="status" data-testid="console-no-tracks">
            {t("portal.traffic.no_tracks", { at: product.at })}
          </p>
        ) : (
          <Table data-testid="console-tracks">
            <TableHeader>
              <TableRow>
                <TableHead>{t("portal.traffic.track")}</TableHead>
                <TableHead>{t("portal.traffic.trust")}</TableHead>
                <TableHead>{t("portal.traffic.state")}</TableHead>
                <TableHead>{t("portal.traffic.age")}</TableHead>
                <TableHead>{t("portal.traffic.alt_amsl")}</TableHead>
                <TableHead>{t("portal.traffic.report")}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {list.map((tr) => {
                const f = facts.get(tr.trackId);
                return (
                  <TableRow key={tr.trackId} data-testid="console-track-row" data-track-id={tr.trackId} data-state={f?.state ?? ""}>
                    <TableCell className="font-mono text-xs">{tr.trackId}</TableCell>
                    <TableCell>
                      {kitT(`trust.${tr.trust}`)} · {tr.source}
                    </TableCell>
                    <TableCell>{f === undefined ? "" : t(`portal.traffic.track_state.${f.state}`)}</TableCell>
                    <TableCell>{f === undefined ? "" : fmtNum(f.ageS, 1, "s", lang)}</TableCell>
                    <TableCell>{fmtNum(tr.altAmslM, 0, "m", lang)}</TableCell>
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
    </section>
  );
}
