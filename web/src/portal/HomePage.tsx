"use client";

// The portal's start page. Signed out: what the portal does and the way
// to sign in or register. Signed in, the overview, the first page after
// sign-in: the operator's intents counted by state as the API counts
// them (GET /v1/intents/summary, on the database clock, "at least" when
// a count reached its bound), the geo-awareness and the weather around
// the map's view (GET /v1/geo, GET /v1/weather, as their pages read
// them), the latest intents (GET /v1/intents), and the operator's record
// (an operator_admin reads it). A figure not read yet is a dash, never a
// zero; nothing is counted here but the API's figures added up.
import Link from "next/link";
import { useCallback, useEffect, useMemo, useState } from "react";
import { ApiError } from "@rootxkit/uspace-ui/api";
import { RequireRole, useSession } from "@rootxkit/uspace-ui/auth/client";
import { fmtAge, fmtTimeUTC, useLang } from "@rootxkit/uspace-ui/i18n";
import { ZoneLayer } from "@rootxkit/uspace-ui/layers";
import { ZoneLegend } from "@rootxkit/uspace-ui/legend";
import type { BBox } from "@rootxkit/uspace-ui/map";
import type { ZoneType, ZoneView } from "@rootxkit/uspace-ui/model";
import type { StatusTone } from "@rootxkit/uspace-ui/theme";
import { Button, EmptyState, KpiRow, MapPanel, PageHeader, Panel, StatTile, StatusBadge } from "@rootxkit/uspace-ui/ui";
import { PageFrame } from "@/components/frame";
import { AircraftIcon, AlertIcon, IntentIcon, PlusIcon, WeatherIcon, ZoneIcon } from "@/components/icons";
import { ProblemNotice } from "@/components/ProblemNotice";
import { useRuntimeConfig } from "@/components/Providers";
import { useAppT } from "@/i18n/t";
import { useApi, type Schemas } from "@/lib/api";
import { Fact, Utc } from "./common";
// The geo page's adapter, which copies the API's fields and computes nothing.
import { toZoneView } from "@/portal/geo/adapt";
import { PortalMap, ViewBox, bboxParam } from "./PortalMap";
import { useMe } from "./useMe";

type Intent = Omit<Schemas["IntentDecision"], "alternative">;
type IntentState = Intent["state"];

/** How many intents the overview lists. Display-only. */
const RECENT = 6;

/** The tone an intent's state is shown with; the words say the state. */
const STATE_TONE: Record<IntentState, StatusTone> = {
  pending_validation: "info",
  pending_dss: "info",
  pending_authority: "info",
  accepted: "ok",
  activated: "ok",
  nonconforming: "critical",
  contingent: "warning",
  ended: "neutral",
  rejected: "warning",
  withdrawn: "neutral",
};
const PENDING: readonly IntentState[] = ["pending_validation", "pending_dss", "pending_authority"];
const ATTENTION: readonly IntentState[] = ["nonconforming", "contingent"];
/** Every state, in the order the counts panel shows them. */
const STATES: readonly IntentState[] = [...PENDING, "accepted", "activated", ...ATTENTION, "ended", "rejected", "withdrawn"];
const WEATHER_TONE: Record<Schemas["WeatherSource"]["state"], StatusTone> = { up: "ok", failing: "critical", never_fetched: "warning" };
const OPERATOR_TONE: Record<Schemas["Operator"]["status"], StatusTone> = { active: "ok", pending_validation: "warning", refused: "critical" };

type Read<T> = { kind: "loading" } | { kind: "failed"; error: unknown } | { kind: "ok"; data: T; atMs: number };

function SignedOut() {
  const t = useAppT();
  return (
    <section aria-labelledby="home-heading" className="flex flex-col gap-6">
      <PageHeader title={<span id="home-heading">{t("portal.home.heading")}</span>} description={t("portal.home.intro")} />
      <Panel>
        <div className="flex flex-col gap-4">
          <p className="m-0 text-sm text-muted-foreground">{t("portal.home.decides")}</p>
          <div className="flex flex-wrap gap-2">
            <Button asChild>
              <Link href="/login">{t("portal.nav.login")}</Link>
            </Button>
            <Button asChild variant="outline">
              <Link href="/register">{t("portal.nav.register")}</Link>
            </Button>
          </div>
        </div>
      </Panel>
    </section>
  );
}

function useIntents(): Read<Intent[]> {
  const api = useApi();
  const [state, setState] = useState<Read<Intent[]>>({ kind: "loading" });
  useEffect(() => {
    let live = true;
    api
      .GET("/v1/intents")
      .then(({ data }) => live && setState({ kind: "ok", data: data?.intents ?? [], atMs: Date.now() }))
      .catch((error: unknown) => live && setState({ kind: "failed", error }));
    return () => {
      live = false;
    };
  }, [api]);
  return state;
}

/** The operator's intents counted by state (the session's operator; the API decides). */
function useSummary(): Read<Schemas["IntentSummary"]> {
  const api = useApi();
  const [state, setState] = useState<Read<Schemas["IntentSummary"]>>({ kind: "loading" });
  useEffect(() => {
    let live = true;
    api
      .GET("/v1/intents/summary")
      .then(({ data }) => live && data !== undefined && setState({ kind: "ok", data, atMs: Date.now() }))
      .catch((error: unknown) => live && setState({ kind: "failed", error }));
    return () => {
      live = false;
    };
  }, [api]);
  return state;
}

/** The geo-awareness and the weather of the box the map shows, as the geo and weather pages read them. */
function useAround(box: string | null) {
  const api = useApi();
  const [geo, setGeo] = useState<Read<Schemas["GeoAnswer"]>>({ kind: "loading" });
  const [weather, setWeather] = useState<Read<Schemas["WeatherAnswer"]> | { kind: "not_configured" }>({ kind: "loading" });
  useEffect(() => {
    if (box === null) return;
    let live = true;
    api
      .GET("/v1/geo", { params: { query: { bbox: box } } })
      .then(({ data }) => live && data !== undefined && setGeo({ kind: "ok", data, atMs: Date.now() }))
      .catch((error: unknown) => live && setGeo({ kind: "failed", error }));
    api
      .GET("/v1/weather", { params: { query: { bbox: box } } })
      .then(({ data }) => live && data !== undefined && setWeather({ kind: "ok", data, atMs: Date.now() }))
      .catch((error: unknown) => {
        if (!live) return;
        if (error instanceof ApiError && error.status === 503 && error.slug === "weather_unavailable") setWeather({ kind: "not_configured" });
        else setWeather({ kind: "failed", error });
      });
    return () => {
      live = false;
    };
  }, [api, box]);
  return { geo, weather };
}

function OperatorPanel({ operatorId }: { operatorId: string }) {
  const t = useAppT();
  const api = useApi();
  const [op, setOp] = useState<Schemas["Operator"] | null>(null);
  const [error, setError] = useState<unknown>(null);
  useEffect(() => {
    let live = true;
    api
      .GET("/v1/accounts/operators/{operator_id}", { params: { path: { operator_id: operatorId } } })
      .then(({ data }) => live && setOp(data ?? null))
      .catch((e: unknown) => live && setError(e));
    return () => {
      live = false;
    };
  }, [api, operatorId]);
  return (
    <Panel
      title={t("portal.overview.operator_title")}
      actions={op === null ? undefined : <StatusBadge tone={OPERATOR_TONE[op.status]}>{t(`portal.operator.status.${op.status}`)}</StatusBadge>}
    >
      {error !== null ? (
        <ProblemNotice error={error} />
      ) : op === null ? (
        <p role="status" className="m-0 text-sm">
          {t("portal.loading")}
        </p>
      ) : (
        <div data-testid="operator" data-status={op.status} className="flex flex-col gap-3">
          <p className="m-0 font-semibold">{op.display_name}</p>
          <dl className="m-0 grid gap-3 sm:grid-cols-2">
            <Fact term={t("portal.register.registration_number")}>{op.registration_number}</Fact>
            <Fact term={t("portal.operator.status")}>{t(`portal.operator.status.${op.status}`)}</Fact>
            <Fact term={t("portal.operator.validation_status")}>{op.validation_status ?? t("portal.operator.validation_none")}</Fact>
            <Fact term={t("portal.operator.validated_at")}>
              <Utc iso={op.validated_at} />
            </Fact>
          </dl>
        </div>
      )}
    </Panel>
  );
}

function Overview() {
  const t = useAppT();
  const { lang } = useLang();
  const cfg = useRuntimeConfig();
  const me = useMe();
  const intents = useIntents();
  const summary = useSummary();
  const [box, setBox] = useState<string | null>(null);
  const onBox = useCallback((b: BBox) => setBox(bboxParam(b)), []);
  const { geo, weather } = useAround(box);
  const [selected, setSelected] = useState<string | null>(null);

  const list = intents.kind === "ok" ? intents.data : null;
  const counts = summary.kind === "ok" ? summary.data.counts : null;
  const count = (states: readonly IntentState[]) => (counts === null ? null : states.reduce((n, st) => n + counts[st], 0));
  const attention = count(ATTENTION);
  const pending = count(PENDING);
  const recent = useMemo(() => (list === null ? [] : [...list].sort((a, b) => b.updated_at.localeCompare(a.updated_at)).slice(0, RECENT)), [list]);
  const intentsHint =
    summary.kind !== "ok"
      ? undefined
      : summary.data.truncated
        ? t("portal.overview.at_least")
        : t("portal.overview.as_of", { time: fmtTimeUTC(summary.data.as_of, lang, { seconds: true }) });
  const intentsFailed = summary.kind === "failed" ? { tone: "critical" as const, label: t("portal.overview.unread") } : undefined;

  const zones: ZoneView[] = useMemo(() => {
    if (geo.kind !== "ok") return [];
    const out: ZoneView[] = [];
    for (const s of geo.data.uspace_airspaces) {
      const v = toZoneView(s, lang);
      if (v !== null) out.push(v);
    }
    for (const z of geo.data.zones) {
      const v = toZoneView(z, lang);
      if (v !== null) out.push(v);
    }
    for (const r of geo.data.restrictions) {
      const v = toZoneView(r, lang, r);
      if (v !== null) out.push(v);
    }
    return out;
  }, [geo, lang]);
  const zoneCounts: Partial<Record<ZoneType, number>> = {};
  for (const z of zones) zoneCounts[z.type] = (zoneCounts[z.type] ?? 0) + 1;
  const geoItems = geo.kind === "ok" ? geo.data.uspace_airspaces.length + geo.data.zones.length + geo.data.restrictions.length : null;

  return (
    <PageFrame
      id="overview-heading"
      title={t("portal.overview.heading")}
      description={t("portal.overview.note")}
      actions={
        <>
          <Button asChild variant="outline" size="sm">
            <Link href="/traffic">{t("portal.overview.open_traffic")}</Link>
          </Button>
          <RequireRole anyOf={["operator_admin", "remote_pilot"]}>
            <Button asChild size="sm">
              <Link href="/intents/new">{t("portal.nav.new_intent")}</Link>
            </Button>
          </RequireRole>
        </>
      }
    >
      {summary.kind === "failed" && <ProblemNotice error={summary.error} />}
      {intents.kind === "failed" && <ProblemNotice error={intents.error} />}
      <KpiRow label={t("portal.overview.kpis")}>
        <StatTile
          icon={<IntentIcon />}
          label={t("portal.overview.intents")}
          value={summary.kind === "ok" ? summary.data.total : null}
          status={intentsFailed}
          hint={intentsHint}
        />
        <StatTile icon={<AircraftIcon />} label={t("portal.state.activated")} value={count(["activated"])} status={intentsFailed} hint={intentsHint} />
        <StatTile icon={<PlusIcon />} label={t("portal.state.accepted")} value={count(["accepted"])} status={intentsFailed} hint={intentsHint} />
        <StatTile
          icon={<IntentIcon />}
          label={t("portal.overview.pending")}
          value={pending}
          status={intentsFailed ?? (pending === null || pending === 0 ? undefined : { tone: "info", label: t("portal.overview.awaiting") })}
          hint={intentsHint}
        />
        <StatTile
          icon={<AlertIcon />}
          label={t("portal.overview.attention")}
          value={attention}
          status={
            intentsFailed ??
            (attention === null ? undefined : attention > 0 ? { tone: "critical", label: t("portal.overview.attention_some") } : { tone: "ok", label: t("portal.overview.attention_none") })
          }
          hint={intentsHint}
        />
        <StatTile
          icon={<ZoneIcon />}
          label={t("portal.overview.geo")}
          value={geoItems}
          status={
            geo.kind === "failed"
              ? { tone: "critical", label: t("portal.overview.unread") }
              : geo.kind === "ok"
                ? geo.data.stale
                  ? { tone: "warning", label: t("portal.overview.stale") }
                  : { tone: "ok", label: t("portal.overview.current") }
                : undefined
          }
          hint={geo.kind === "ok" ? t("portal.overview.cis", { age: fmtAge(geo.data.cis_age_s, lang) }) : undefined}
        />
        <StatTile
          icon={<WeatherIcon />}
          label={t("portal.overview.weather")}
          value={weather.kind === "ok" ? weather.data.products.length : null}
          status={
            weather.kind === "failed"
              ? { tone: "critical", label: t("portal.overview.unread") }
              : weather.kind === "ok"
                ? weather.data.stale
                  ? { tone: "warning", label: t("portal.overview.stale") }
                  : { tone: WEATHER_TONE[weather.data.source.state], label: t(`portal.weather.source_state.${weather.data.source.state}`) }
                : weather.kind === "not_configured"
                  ? { tone: "neutral", label: t("portal.overview.not_configured") }
                  : undefined
          }
          hint={weather.kind === "ok" ? t("portal.overview.weather_source", { name: weather.data.source.name }) : undefined}
        />
      </KpiRow>

      <div className="grid gap-6 xl:grid-cols-[minmax(0,1fr)_26rem]">
        <MapPanel
          title={t("portal.overview.map_title")}
          description={t("portal.overview.map_note")}
          mapClassName="h-[24rem] lg:h-[30rem]"
          legend={<ZoneLegend counts={zoneCounts} />}
          actions={
            <Button asChild variant="outline" size="sm">
              <Link href="/geo">{t("portal.overview.open_geo")}</Link>
            </Button>
          }
        >
          <div className="absolute inset-0">
            <PortalMap className="h-full">
              <ViewBox margin={cfg.geoMarginFraction} onChange={onBox} />
              <ZoneLayer id="overview-geo" zones={zones} selectedId={selected} onSelect={setSelected} />
            </PortalMap>
          </div>
        </MapPanel>
        <Panel
          title={t("portal.overview.recent_title")}
          description={t("portal.overview.recent_note")}
          actions={
            <Button asChild variant="outline" size="sm">
              <Link href="/intents">{t("portal.overview.all_intents")}</Link>
            </Button>
          }
        >
          {list === null ? (
            <p role="status" className="m-0 text-sm">
              {intents.kind === "failed" ? t("portal.overview.unread") : t("portal.loading")}
            </p>
          ) : recent.length === 0 ? (
            <EmptyState icon={<IntentIcon />} title={t("portal.intents.none")} />
          ) : (
            <ul className="m-0 grid list-none gap-0 p-0" data-testid="recent-intents">
              {recent.map((d) => (
                <li key={d.intent_id} className="flex flex-col gap-1 border-b border-border py-2.5 text-sm first:pt-0 last:border-b-0 last:pb-0">
                  <div className="flex flex-wrap items-center justify-between gap-2">
                    <Link href={`/intents/${d.intent_id}`} className="min-w-0 truncate font-mono text-xs font-semibold underline-offset-4 hover:underline">
                      {d.authorisation_number ?? d.intent_id}
                    </Link>
                    <StatusBadge tone={STATE_TONE[d.state]}>{t(`portal.state.${d.state}`)}</StatusBadge>
                  </div>
                  <p className="m-0 text-xs text-muted-foreground">
                    <Utc iso={d.valid_from} /> – <Utc iso={d.valid_to} />
                  </p>
                </li>
              ))}
            </ul>
          )}
        </Panel>
      </div>

      <Panel title={t("portal.overview.counts_title")} description={t("portal.overview.counts_note")}>
        {counts === null ? (
          <p role="status" className="m-0 text-sm">
            {summary.kind === "failed" ? t("portal.overview.unread") : t("portal.loading")}
          </p>
        ) : (
          <dl
            className="m-0 grid gap-3 sm:grid-cols-3 lg:grid-cols-5"
            data-testid="intent-counts"
            data-total={summary.kind === "ok" ? summary.data.total : undefined}
            data-truncated={summary.kind === "ok" ? String(summary.data.truncated) : undefined}
          >
            {STATES.map((st) => (
              <Fact key={st} term={t(`portal.state.${st}`)} testId={`intent-count-${st}`}>
                <span className="text-lg font-semibold tabular-nums">{counts[st]}</span>
              </Fact>
            ))}
          </dl>
        )}
        {summary.kind === "ok" && summary.data.truncated && <p className="m-0 mt-3 text-sm">{t("portal.overview.at_least")}</p>}
      </Panel>

      <div className="grid items-start gap-6 lg:grid-cols-2">
        <Panel title={t("portal.overview.planning_title")} description={t("portal.overview.planning_note")}>
          <dl className="m-0 grid gap-3 sm:grid-cols-2">
            <Fact term={t("portal.overview.cis_version")}>{geo.kind === "ok" ? geo.data.cis_version : t("portal.overview.unknown")}</Fact>
            <Fact term={t("portal.overview.cis_age")}>{geo.kind === "ok" ? fmtAge(geo.data.cis_age_s, lang) : t("portal.overview.unknown")}</Fact>
            <Fact term={t("portal.weather.source")}>
              {weather.kind === "ok" ? weather.data.source.name : weather.kind === "not_configured" ? t("portal.overview.not_configured") : t("portal.overview.unknown")}
            </Fact>
            <Fact term={t("portal.weather.last_success")}>{weather.kind === "ok" ? <Utc iso={weather.data.source.last_success_at} /> : t("portal.overview.unknown")}</Fact>
          </dl>
          {geo.kind === "failed" && <ProblemNotice error={geo.error} />}
          {weather.kind === "failed" && <ProblemNotice error={weather.error} />}
        </Panel>
        {me.kind === "loaded" && me.me.operator_id !== undefined && me.me.roles.includes("operator_admin") ? (
          <OperatorPanel operatorId={me.me.operator_id} />
        ) : me.kind === "failed" ? (
          <ProblemNotice error={me.error} />
        ) : null}
      </div>
    </PageFrame>
  );
}

export function HomePage() {
  const { session } = useSession();
  return session === null ? <SignedOut /> : <Overview />;
}
