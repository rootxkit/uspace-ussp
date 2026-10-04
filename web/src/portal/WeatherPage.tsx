"use client";

// Weather information for the planning area (Art. 12, optional; brief
// WP-16, WP-17): GET /v1/weather for the box around the map's first
// view. The products as stored, each with its station, kind, the
// Art. 12(2) fields with their units (wind in degrees true and m/s, the
// ceiling in feet above the aerodrome as reported, visibility in
// metres), age and validity, and the report as received; the source's
// state. No source configured is the API's 503 weather_unavailable,
// shown as "not configured"; a failing or old source answers its last
// products marked stale. Nothing is computed here.
import { useCallback, useEffect, useState } from "react";
import { ApiError } from "@rootxkit/uspace-ui/api";
import { fmtAge, useLang } from "@rootxkit/uspace-ui/i18n";
import type { BBox } from "@rootxkit/uspace-ui/map";
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@rootxkit/uspace-ui/ui";
import { ProblemNotice } from "@/components/ProblemNotice";
import { useRuntimeConfig } from "@/components/Providers";
import { useAppT } from "@/i18n/t";
import { useApi, type Schemas } from "@/lib/api";
import { Fact, Num, PageHeading, RequireSession, Utc } from "./common";
import { PortalMap, ViewBox, bboxParam } from "./PortalMap";

function Fields({ f }: { f: Schemas["WeatherFields"] }) {
  const t = useAppT();
  return (
    <dl className="m-0 grid grid-cols-2 gap-x-3 gap-y-0.5 text-xs">
      <dt>{t("portal.weather.wind")}</dt>
      <dd className="m-0">
        {f.wind_variable ? t("portal.weather.variable") : <Num v={f.wind_dir_deg} digits={0} unit="°" />} / <Num v={f.wind_speed_ms} unit="m/s" />
        {f.gust_ms !== null && (
          <>
            {" "}
            {t("portal.weather.gust")} <Num v={f.gust_ms} unit="m/s" />
          </>
        )}
      </dd>
      <dt>{t("portal.weather.visibility")}</dt>
      <dd className="m-0">
        {f.visibility_at_least && "≥ "}
        <Num v={f.visibility_m} digits={0} unit="m" />
      </dd>
      <dt>{t("portal.weather.ceiling")}</dt>
      <dd className="m-0">
        {t(`portal.weather.ceiling.${f.ceiling}`)} <Num v={f.cloud_base_ft_agl} digits={0} unit="ft AGL" />
      </dd>
      <dt>{t("portal.weather.temp")}</dt>
      <dd className="m-0">
        <Num v={f.temp_c} digits={0} unit="°C" /> / <Num v={f.dew_point_c} digits={0} unit="°C" />
      </dd>
      <dt>{t("portal.weather.qnh")}</dt>
      <dd className="m-0">
        <Num v={f.qnh_hpa} digits={0} unit="hPa" />
      </dd>
      <dt>{t("portal.weather.indicators")}</dt>
      <dd className="m-0">
        {t(f.convective ? "portal.weather.convective" : "portal.weather.no_convective")}; {t(f.precipitation ? "portal.weather.precipitation" : "portal.weather.no_precipitation")}
      </dd>
    </dl>
  );
}

type Loaded = { kind: "loading" } | { kind: "not_configured"; error: ApiError } | { kind: "failed"; error: unknown } | { kind: "ok"; answer: Schemas["WeatherAnswer"]; bbox: string };

export function WeatherPage() {
  const t = useAppT();
  const { lang } = useLang();
  const api = useApi();
  const cfg = useRuntimeConfig();
  const [state, setState] = useState<Loaded>({ kind: "loading" });
  const [bbox, setBbox] = useState<string | null>(null);
  const onBox = useCallback((b: BBox) => setBbox(bboxParam(b)), []);
  useEffect(() => {
    if (bbox === null) return;
    let live = true;
    api
      .GET("/v1/weather", { params: { query: { bbox } } })
      .then(({ data }) => live && data !== undefined && setState({ kind: "ok", answer: data, bbox }))
      .catch((error: unknown) => {
        if (!live) return;
        if (error instanceof ApiError && error.status === 503 && error.slug === "weather_unavailable") setState({ kind: "not_configured", error });
        else setState({ kind: "failed", error });
      });
    return () => {
      live = false;
    };
  }, [api, bbox]);
  return (
    <section aria-labelledby="weather-heading" className="flex flex-col gap-3">
      <PageHeading id="weather-heading">{t("portal.weather.heading")}</PageHeading>
      <p className="m-0 text-sm text-[var(--us-text-muted)]">{t("portal.weather.note")}</p>
      <RequireSession>
        <PortalMap className="h-[280px]">
          <ViewBox margin={cfg.geoMarginFraction} onChange={onBox} />
        </PortalMap>
        {state.kind === "loading" && <p role="status">{t("portal.loading")}</p>}
        {state.kind === "failed" && <ProblemNotice error={state.error} />}
        {state.kind === "not_configured" && (
          <div role="status" data-testid="weather-not-configured" className="flex flex-col gap-1">
            <p className="m-0 font-semibold">{t("portal.weather.not_configured")}</p>
            <ProblemNotice error={state.error} testId="weather-problem" />
          </div>
        )}
        {state.kind === "ok" && (
          <>
            <dl className="m-0 grid gap-2 text-sm sm:grid-cols-4" data-testid="weather-source" data-state={state.answer.source.state}>
              <Fact term={t("portal.weather.source")}>{state.answer.source.name}</Fact>
              <Fact term={t("portal.weather.source_state")}>{t(`portal.weather.source_state.${state.answer.source.state}`)}</Fact>
              <Fact term={t("portal.weather.last_success")}>
                <Utc iso={state.answer.source.last_success_at} /> ({fmtAge(state.answer.source.age_s, lang)})
              </Fact>
              <Fact term={t("portal.weather.box")}>{state.bbox}</Fact>
            </dl>
            {state.answer.stale && (
              <p role="alert" data-testid="weather-stale" className="m-0 rounded border-2 border-[var(--us-severity-warning)] p-2 text-sm font-semibold">
                {t("portal.weather.stale")}
                {state.answer.source.failure !== null && (
                  <>
                    {" "}
                    {t("portal.weather.failing", { failure: state.answer.source.failure })} <Utc iso={state.answer.source.last_failure_at} />
                  </>
                )}
              </p>
            )}
            {state.answer.products.length === 0 ? (
              <p role="status" data-testid="weather-none">
                {t("portal.weather.none")}
              </p>
            ) : (
              <Table data-testid="weather-products">
                <TableHeader>
                  <TableRow>
                    <TableHead>{t("portal.weather.station")}</TableHead>
                    <TableHead>{t("portal.weather.observed")}</TableHead>
                    <TableHead>{t("portal.weather.validity")}</TableHead>
                    <TableHead>{t("portal.weather.fields")}</TableHead>
                    <TableHead>{t("portal.weather.raw")}</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {state.answer.products.map((p) => (
                    <TableRow key={p.id} data-testid="weather-product">
                      <TableCell>
                        {p.station} · {p.kind.toUpperCase()}
                        {!p.in_force && <p className="m-0 text-xs">{t("portal.weather.not_in_force")}</p>}
                      </TableCell>
                      <TableCell>
                        <Utc iso={p.observed_at} /> ({fmtAge(p.age_s, lang)})
                      </TableCell>
                      <TableCell>
                        <Utc iso={p.valid_from} /> – <Utc iso={p.valid_to} />
                      </TableCell>
                      <TableCell>
                        <Fields f={p.fields} />
                        {p.changes.length > 0 && <p className="m-0 mt-1 text-xs">{t("portal.weather.changes", { n: p.changes.length })}</p>}
                      </TableCell>
                      <TableCell className="font-mono text-xs">{p.raw}</TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            )}
          </>
        )}
      </RequireSession>
    </section>
  );
}
