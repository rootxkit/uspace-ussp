"use client";

// The console's overview, its first page after sign-in (brief WP-18):
// the key figures of the service now, read from the console's own API
// operations (flights, alerts, escalations, emergency cases, inputs, the
// DSS) and the live stream; then the live map, traffic-ws's stream with
// the staff session cookie on a same-origin upgrade (M22), subscribed by
// the map's viewport (console/subscribe/v1 {bbox, layers} on every
// settled move, M29); its traffic/product/v1 bodies on the map with the
// kit's symbology and in a list with each track's state and age (a
// switched-off source's tracks show source_disabled and then age out),
// the stream's degraded inputs with their time, the CPA thresholds in
// force, the inputs, and the escalated alerts the supervisor must handle.
// A figure that could not be read is a dash with the reason, never a
// zero. It shows and decides nothing.
import Link from "next/link";
import { useCallback, useMemo, useState, type ReactNode } from "react";
import { fmtNum, fmtTimeUTC, useLang, useT } from "@rootxkit/uspace-ui/i18n";
import { TrackLayer } from "@rootxkit/uspace-ui/layers";
import { AgeLegend, IdentificationLegend, TrackLegend } from "@rootxkit/uspace-ui/legend";
import { createTrackStore, subscribeFrame, useFeed, useNowMs, useStore, type ConsoleFrame } from "@rootxkit/uspace-ui/live";
import type { BBox } from "@rootxkit/uspace-ui/map";
import type { Trust } from "@rootxkit/uspace-ui/model";
import { FeedStatusBar, ThresholdsPanel } from "@rootxkit/uspace-ui/status";
import type { StatusTone } from "@rootxkit/uspace-ui/theme";
import {
  Button,
  KpiRow,
  MapPanel,
  Panel,
  StatTile,
  StatusBadge,
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
  type StatTileStatus,
} from "@rootxkit/uspace-ui/ui";
import { useRouter } from "next/navigation";
import { PageFrame, useShellFeed } from "@/components/frame";
import { AircraftIcon, AlertIcon, EmergencyIcon, EscalationIcon, FlightIcon, NetworkIcon, PolicyIcon, SourceIcon } from "@/components/icons";
import { useRuntimeConfig } from "@/components/Providers";
import { useAppT, type AppT } from "@/i18n/t";
import { useConsoleApi } from "@/lib/consoleApi";
import { Utc } from "@/portal/common";
import { PortalMap, ViewBox } from "@/portal/PortalMap";
import { PRODUCT_SCHEMA, adaptProduct, type Product, type TrackFacts } from "@/portal/live/adapt";
import { DegradedList, StreamFacts } from "@/portal/live/StreamFacts";
import { MAX_TRACKS, TRAIL_POINTS, type StreamCounters } from "@/portal/live/useStreams";
import { EscalationsPanel, type Escalations } from "./EscalationsPanel";
import { LOGIN } from "./ConsoleShell";
import { inputName, useInputState, useInputs } from "./InputsStrip";
import { body, usePoll, type Poll } from "./usePoll";

const TICK_MS = 1000;
/** How often the overview's figures are read. Display-only. */
const OVERVIEW_PERIOD_MS = 5000;
/** How many inputs the overview's inputs panel lists. Display-only. */
const INPUTS_LISTED = 8;
const ZERO: StreamCounters = { productsRefused: 0, tracksRefused: 0, alertsRefused: 0, framesNotShown: 0, alertsEvicted: 0 };

const DEP_TONE: Record<"up" | "degraded" | "down" | "unknown", StatusTone> = { up: "ok", degraded: "warning", down: "critical", unknown: "neutral" };
const MONITOR_TONE: Record<"up" | "down" | "unknown", StatusTone> = { up: "ok", down: "critical", unknown: "neutral" };
const INPUT_TONE: Record<"healthy" | "stale" | "lagging" | "unreachable" | "never_heard" | "disabled" | "unknown", StatusTone> = {
  healthy: "ok",
  stale: "warning",
  lagging: "warning",
  unreachable: "critical",
  never_heard: "warning",
  disabled: "neutral",
  unknown: "neutral",
};

function asOf(t: AppT, lang: ReturnType<typeof useLang>["lang"], ms: number | null): string | undefined {
  return ms === null ? undefined : t("console.overview.as_of", { time: fmtTimeUTC(new Date(ms).toISOString(), lang, { seconds: true }) });
}

/** A read that fails says so in the tile; a read that has not answered yet is a dash. */
function unread<T>(t: AppT, p: Poll<T>): StatTileStatus | undefined {
  return p.failingSinceMs === null ? undefined : { tone: "critical", label: t("console.overview.unread") };
}

/** The tile of a count: a dash before the first answer, the API's number after it. */
function countTile(
  t: AppT,
  lang: ReturnType<typeof useLang>["lang"],
  p: Poll<unknown>,
  props: { icon: ReactNode; label: string; value: number | null; truncated: boolean; status?: StatTileStatus },
) {
  const failing = unread(t, p);
  return (
    <StatTile
      icon={props.icon}
      label={props.label}
      value={props.value}
      status={failing ?? props.status}
      hint={props.truncated ? t("console.overview.truncated") : asOf(t, lang, p.okAtMs)}
    />
  );
}

function OverviewKpis({ tracks, escalations, escalationsFailing }: { tracks: number | null; escalations: Escalations | null; escalationsFailing: number | null }) {
  const t = useAppT();
  const { lang } = useLang();
  const api = useConsoleApi();
  const inputs = useInputs();
  const flights = usePoll(
    useCallback(() => api.GET("/v1/admin/flights").then(body), [api]),
    OVERVIEW_PERIOD_MS,
  );
  const alerts = usePoll(
    useCallback(() => api.GET("/v1/admin/alerts", { params: { query: { view: "active" } } }).then(body), [api]),
    OVERVIEW_PERIOD_MS,
  );
  const cases = usePoll(
    useCallback(() => api.GET("/v1/admin/emergency").then(body), [api]),
    OVERVIEW_PERIOD_MS,
  );
  const dss = usePoll(
    useCallback(() => api.GET("/v1/admin/dss").then(body), [api]),
    OVERVIEW_PERIOD_MS,
  );
  const nAlerts = alerts.data?.alerts.length ?? null;
  const nEsc = escalations?.alerts.length ?? null;
  const nCases = cases.data === null ? null : cases.data.cases.filter((c) => c.closed_at === undefined).length;
  const sources = inputs.data?.sources ?? null;
  const healthy = sources === null ? null : sources.filter((s) => s.state === "healthy").length;
  const switchedOn = sources === null ? null : sources.filter((s) => s.state !== "disabled").length;
  const switchedOff = sources === null || switchedOn === null ? 0 : sources.length - switchedOn;
  const monitor = inputs.data?.monitor.state ?? null;
  const dssState = dss.data?.readiness.state ?? null;
  return (
    <KpiRow label={t("console.overview.kpis")}>
      <StatTile
        icon={<AircraftIcon />}
        label={t("console.overview.tracks")}
        value={tracks}
        hint={tracks === null ? t("portal.traffic.waiting") : t("console.overview.tracks_hint")}
      />
      {countTile(t, lang, flights, {
        icon: <FlightIcon />,
        label: t("console.overview.flights"),
        value: flights.data?.flights.length ?? null,
        truncated: flights.data?.truncated === true,
      })}
      {countTile(t, lang, alerts, {
        icon: <AlertIcon />,
        label: t("console.overview.alerts"),
        value: nAlerts,
        truncated: alerts.data?.truncated === true,
        ...(nAlerts === null ? {} : { status: nAlerts > 0 ? { tone: "warning", label: t("console.overview.open") } : { tone: "ok", label: t("console.overview.none_open") } }),
      })}
      <StatTile
        icon={<EscalationIcon />}
        label={t("console.overview.escalations")}
        value={escalationsFailing !== null ? null : nEsc}
        status={
          escalationsFailing !== null
            ? { tone: "critical", label: t("console.overview.unread") }
            : nEsc === null
              ? undefined
              : nEsc > 0
                ? { tone: "critical", label: t("console.overview.to_handle") }
                : { tone: "ok", label: t("console.overview.none_to_handle") }
        }
        hint={escalations?.truncated === true ? t("console.overview.truncated") : undefined}
      />
      {countTile(t, lang, cases, {
        icon: <EmergencyIcon />,
        label: t("console.overview.cases"),
        value: nCases,
        truncated: cases.data?.truncated === true,
        ...(nCases === null ? {} : { status: nCases > 0 ? { tone: "critical", label: t("console.overview.open") } : { tone: "ok", label: t("console.overview.none_open") } }),
      })}
      <StatTile
        icon={<SourceIcon />}
        label={t("console.overview.inputs")}
        value={healthy === null || switchedOn === null ? null : t("console.overview.inputs_value", { healthy, total: switchedOn })}
        status={
          inputs.failingSinceMs !== null
            ? { tone: "critical", label: t("console.overview.unread") }
            : inputs.data === null
              ? undefined
              : inputs.data.bus.state !== "connected"
                ? { tone: "critical", label: t("console.overview.bus_down") }
                : healthy === switchedOn
                  ? { tone: "ok", label: t("console.overview.all_healthy") }
                  : { tone: "warning", label: t("console.overview.not_all_healthy") }
        }
        hint={
          inputs.data?.sources_truncated === true
            ? t("console.overview.truncated")
            : switchedOff > 0
              ? t("console.overview.switched_off", { n: switchedOff })
              : asOf(t, lang, inputs.okAtMs)
        }
      />
      <StatTile
        icon={<PolicyIcon />}
        label={t("console.overview.monitor")}
        value={monitor === null ? null : t(`console.overview.monitor.${monitor}`)}
        {...(monitor === null ? {} : { status: { tone: MONITOR_TONE[monitor], label: t(`console.overview.monitor_status.${monitor}`) } })}
        hint={asOf(t, lang, inputs.okAtMs)}
      />
      <StatTile
        icon={<NetworkIcon />}
        label={t("console.overview.dss")}
        value={dssState === null ? null : t(`console.overview.dep.${dssState}`)}
        status={unread(t, dss) ?? (dssState === null ? undefined : { tone: DEP_TONE[dssState], label: t(`console.overview.dep.${dssState}`) })}
        hint={dss.data === null ? undefined : t("console.overview.since", { time: fmtTimeUTC(dss.data.readiness.since, lang, { seconds: true }) })}
      />
    </KpiRow>
  );
}

/** The inputs at a glance: the first ones as the strip reads them, each with its state; the rest on the inputs page. */
function InputsGlance() {
  const t = useAppT();
  const stateWord = useInputState();
  const { data } = useInputs();
  return (
    <Panel
      title={t("console.overview.inputs_title")}
      description={t("console.overview.inputs_note")}
      actions={
        <Button asChild variant="outline" size="sm">
          <Link href="/console/inputs">{t("console.overview.all_inputs")}</Link>
        </Button>
      }
    >
      {data === null ? (
        <p className="m-0 text-sm">{t("portal.loading")}</p>
      ) : (
        <ul className="m-0 grid list-none gap-0 p-0">
          {data.sources.slice(0, INPUTS_LISTED).map((i) => (
            <li key={inputName(i)} className="flex flex-wrap items-center justify-between gap-x-3 gap-y-1 border-b border-border py-2 text-sm last:border-b-0">
              <span className="min-w-0 truncate font-mono text-xs">{inputName(i)}</span>
              <StatusBadge tone={INPUT_TONE[i.state]}>{stateWord(i.state)}</StatusBadge>
            </li>
          ))}
        </ul>
      )}
    </Panel>
  );
}

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
  const [escalations, setEscalations] = useState<{ data: Escalations | null; failingSinceMs: number | null }>({ data: null, failingSinceMs: null });
  const onEscalations = useCallback((data: Escalations | null, failingSinceMs: number | null) => setEscalations({ data, failingSinceMs }), []);
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
  useShellFeed(feed);
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
    <PageFrame id="overview-heading" title={t("console.overview.heading")} description={t("console.overview.note")}>
      <OverviewKpis tracks={product === null ? null : list.length} escalations={escalations.data} escalationsFailing={escalations.failingSinceMs} />
      <section
        aria-label={t("portal.feed.label")}
        className="flex flex-wrap items-start gap-x-6 gap-y-1 rounded-panel border border-border bg-card px-4 py-2.5 text-xs shadow-panel"
        data-testid="console-feed"
        data-connection={feed.connection}
      >
        <FeedStatusBar status={feed} nowMs={nowMs} />
        <StreamFacts feed={feed} droppedFrames={product?.droppedFrames ?? null} counters={counters} at={product?.at ?? null} policyVersion={product?.policyVersion ?? null} cisVersion={product?.cisVersion ?? null} />
      </section>
      <DegradedList degraded={product?.degraded ?? null} feedDegraded={feed.degraded} />
      <div className="grid gap-6 xl:grid-cols-[minmax(0,1fr)_24rem]">
        <MapPanel
          title={t("console.map.heading")}
          description={t("console.map.note")}
          mapClassName="h-[26rem] lg:h-[34rem]"
          legend={
            <div className="flex flex-col gap-2">
              <TrackLegend counts={trustCounts} />
              <IdentificationLegend />
              {feed.staleAfterS !== null && <AgeLegend staleAfterS={feed.staleAfterS} />}
            </div>
          }
        >
          <div className="absolute inset-0">
            <PortalMap className="h-full">
              <ViewBox margin={cfg.geoMarginFraction} onChange={onBox} />
              {feed.staleAfterS !== null && (
                <TrackLayer tracks={list} staleAfterS={feed.staleAfterS} nowMs={nowMs} selectedId={selected} onSelect={setSelected} trails={{ points: TRAIL_POINTS }} labels />
              )}
            </PortalMap>
          </div>
        </MapPanel>
        <div className="flex min-w-0 flex-col gap-6">
          {/* The CPA thresholds the monitor judges with, as the status frame carries them (audit M-5). */}
          <ThresholdsPanel thresholds={feed.extras.thresholds} evaluationPeriodS={feed.extras.evaluationPeriodS} policyVersion={feed.policyVersion} />
          <InputsGlance />
        </div>
      </div>
      <EscalationsPanel onData={onEscalations} />
      <Panel title={t("portal.traffic.tracks_heading", { n: list.length })} padding={product === null || list.length === 0 ? "default" : "none"}>
        {product === null ? (
          <p role="status" className="m-0" data-testid="console-no-product">
            {t("portal.traffic.waiting")}
          </p>
        ) : list.length === 0 ? (
          <p role="status" className="m-0" data-testid="console-no-tracks">
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
      </Panel>
    </PageFrame>
  );
}
