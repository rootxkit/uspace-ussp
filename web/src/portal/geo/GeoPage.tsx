"use client";

// Geo-awareness (Art. 9; brief WP-17): what the CIS says around the map
// (GET /v1/geo?bbox=) or around one of the operator's intents (GET
// /v1/geo/intents/{id}), drawn with the kit's zone symbology and listed
// with every item's version, updated_at and validity, how it applies
// (applies, or unknown with why), the U-space airspaces with their
// Art. 3(4) requirements, and the restrictions with their state and
// window. A stale cache still answers and the page says how old it is;
// a cut list says it was cut. For an intent, the traffic stream's
// geo/changed/v1 frame refetches the answer. Nothing is judged here.
import { useCallback, useEffect, useMemo, useState } from "react";
import { ZoneLayer } from "@rootxkit/uspace-ui/layers";
import { ZoneLegend } from "@rootxkit/uspace-ui/legend";
import type { BBox } from "@rootxkit/uspace-ui/map";
import { fmtAge, useLang } from "@rootxkit/uspace-ui/i18n";
import type { ZoneType, ZoneView } from "@rootxkit/uspace-ui/model";
import { Button } from "@rootxkit/uspace-ui/ui";
import { ProblemNotice } from "@/components/ProblemNotice";
import { useRuntimeConfig } from "@/components/Providers";
import { useAppT } from "@/i18n/t";
import { useApi, type Schemas } from "@/lib/api";
import { Fact, PageHeading, RequireSession, Utc } from "../common";
import { IntentPicker } from "../IntentPicker";
import { useTraffic } from "../live/useStreams";
import { PortalMap, ViewBox, bboxParam } from "../PortalMap";
import { toZoneView } from "./adapt";

type Answer = Schemas["GeoAnswer"];

function Applicability({ a }: { a: Schemas["GeoApplicability"] }) {
  const t = useAppT();
  return (
    <>
      {t(`portal.geo.applicability.${a.kind}`)}
      {(a.from !== null || a.to !== null) && (
        <>
          {" "}
          (<Utc iso={a.from} /> – <Utc iso={a.to} />)
        </>
      )}
      {a.at_state !== undefined && <span>; {t(`portal.geo.at_state.${a.at_state}`)}</span>}
      {a.why !== undefined && <span>: {a.why}</span>}
    </>
  );
}

function Parts({ parts }: { parts: Schemas["GeoPart"][] }) {
  const t = useAppT();
  const { lang } = useLang();
  const lim = (l: Schemas["GeoLimit"] | null) => (l === null ? t("portal.geo.no_limit") : `${new Intl.NumberFormat(lang === "ka" ? "ka-GE" : "en-GB").format(l.value_m)} m ${l.ref}`);
  return (
    <ul className="m-0 pl-4 text-xs">
      {parts.map((p) => (
        <li key={p.id}>
          {t("portal.geo.part", { id: p.id, lower: lim(p.lower), upper: lim(p.upper) })}
        </li>
      ))}
    </ul>
  );
}

function ItemCard({ item, children }: { item: Schemas["GeoItem"]; children?: React.ReactNode }) {
  const t = useAppT();
  return (
    <li className="rounded border border-[var(--us-border)] p-2 text-sm" data-testid="geo-item" data-identifier={item.identifier} data-dataset={item.dataset}>
      <p className="m-0 font-semibold">
        {item.identifier} · {item.type}
      </p>
      <dl className="m-0 mt-1 grid gap-1 text-xs sm:grid-cols-2">
        <Fact term={t("portal.geo.version")}>{item.version}</Fact>
        <Fact term={t("portal.geo.updated_at")}>
          <Utc iso={item.updated_at} />
        </Fact>
        <Fact term={t("portal.geo.valid_from")}>
          <Utc iso={item.valid_from} />
        </Fact>
        <Fact term={t("portal.geo.valid_to")}>
          <Utc iso={item.valid_to} />
        </Fact>
        <Fact term={t("portal.geo.applicability")}>
          <Applicability a={item.applicability} />
        </Fact>
      </dl>
      <Parts parts={item.parts} />
      {children}
    </li>
  );
}

function AnswerView({ a }: { a: Answer }) {
  const t = useAppT();
  const { lang } = useLang();
  return (
    <div className="flex flex-col gap-3">
      {a.stale && (
        <p role="alert" data-testid="geo-stale" className="m-0 rounded border-2 border-[var(--us-severity-warning)] p-2 text-sm font-semibold">
          {t("portal.geo.stale", { age: fmtAge(a.cis_age_s, lang) })}
        </p>
      )}
      <dl className="m-0 grid gap-2 text-sm sm:grid-cols-3" data-testid="geo-basis">
        <Fact term={t("portal.geo.cis_version")}>{a.cis_version === "" ? t("portal.geo.nothing_loaded") : a.cis_version}</Fact>
        <Fact term={t("portal.geo.cis_age")}>{fmtAge(a.cis_age_s, lang)}</Fact>
        <Fact term={t("portal.geo.at")}>
          {a.at !== null ? (
            <Utc iso={a.at} />
          ) : (
            <>
              <Utc iso={a.from} /> – <Utc iso={a.to} />
            </>
          )}
        </Fact>
      </dl>
      {a.truncated && <p role="status">{t("portal.list.truncated")}</p>}
      <section aria-labelledby="airspaces-heading">
        <h2 id="airspaces-heading" className="m-0 mb-1 text-base font-semibold">
          {t("portal.geo.airspaces", { n: a.uspace_airspaces.length })}
        </h2>
        <ul className="m-0 flex list-none flex-col gap-2 p-0">
          {a.uspace_airspaces.map((s) => (
            <ItemCard key={`${s.dataset}-${s.identifier}`} item={s}>
              <p className="m-0 mt-1 text-xs">{t("portal.geo.services_required", { services: s.services_required.join(", ") })}</p>
              {s.adjacent.length > 0 && <p className="m-0 text-xs">{t("portal.geo.adjacent", { ids: s.adjacent.join(", ") })}</p>}
              {s.requirements_problem !== null && (
                <p role="alert" className="m-0 text-xs font-semibold">
                  {t("portal.geo.requirements_problem", { problem: s.requirements_problem })}
                </p>
              )}
              {s.requirements !== null && (
                <details className="mt-1 text-xs">
                  <summary className="cursor-pointer">{t("portal.geo.requirements")}</summary>
                  <pre className="m-0 overflow-x-auto whitespace-pre-wrap">{JSON.stringify(s.requirements, null, 2)}</pre>
                </details>
              )}
            </ItemCard>
          ))}
        </ul>
      </section>
      <section aria-labelledby="zones-heading">
        <h2 id="zones-heading" className="m-0 mb-1 text-base font-semibold">
          {t("portal.geo.zones", { n: a.zones.length })}
        </h2>
        <ul className="m-0 flex list-none flex-col gap-2 p-0">
          {a.zones.map((z) => (
            <ItemCard key={`${z.dataset}-${z.identifier}`} item={z} />
          ))}
        </ul>
      </section>
      <section aria-labelledby="restrictions-heading">
        <h2 id="restrictions-heading" className="m-0 mb-1 text-base font-semibold">
          {t("portal.geo.restrictions", { n: a.restrictions.length })}
        </h2>
        <ul className="m-0 flex list-none flex-col gap-2 p-0">
          {a.restrictions.map((r) => (
            <ItemCard key={`${r.dataset}-${r.identifier}`} item={r}>
              <p className="m-0 mt-1 text-xs">
                {t("portal.geo.restriction_state", { state: r.state ?? t("portal.geo.not_given"), ansp: r.ansp_ref ?? t("portal.geo.not_given") })}{" "}
                <Utc iso={r.starts_at} /> – <Utc iso={r.ends_at} />
              </p>
            </ItemCard>
          ))}
        </ul>
      </section>
    </div>
  );
}

function GeoChanges({ intentId, onChanged }: { intentId: string; onChanged(): void }) {
  const t = useAppT();
  const s = useTraffic(intentId, onChanged);
  return (
    <p className="m-0 text-xs" role="status" data-testid="geo-live" data-connection={s.feed.connection}>
      {t("portal.geo.live", { state: t(`portal.feed.connection.${s.feed.connection}`), n: s.geoChanges })}
    </p>
  );
}

export function GeoPage({ intentId }: { intentId: string | null }) {
  const t = useAppT();
  const { lang } = useLang();
  const api = useApi();
  const cfg = useRuntimeConfig();
  const [box, setBox] = useState<string | null>(null);
  const onBox = useCallback((b: BBox) => setBox(bboxParam(b)), []);
  const [answer, setAnswer] = useState<Answer | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [selected, setSelected] = useState<string | null>(null);

  const fetchAnswer = useCallback(() => {
    const req =
      intentId !== null
        ? api.GET("/v1/geo/intents/{intent_id}", { params: { path: { intent_id: intentId } } })
        : box !== null
          ? api.GET("/v1/geo", { params: { query: { bbox: box } } })
          : null;
    if (req === null) return;
    req
      .then(({ data }) => {
        setAnswer(data ?? null);
        setError(null);
      })
      .catch(setError);
  }, [api, box, intentId]);
  useEffect(fetchAnswer, [fetchAnswer]);

  const zones: ZoneView[] = useMemo(() => {
    if (answer === null) return [];
    const out: ZoneView[] = [];
    for (const s of answer.uspace_airspaces) {
      const v = toZoneView(s, lang);
      if (v !== null) out.push(v);
    }
    for (const z of answer.zones) {
      const v = toZoneView(z, lang);
      if (v !== null) out.push(v);
    }
    for (const r of answer.restrictions) {
      const v = toZoneView(r, lang, r);
      if (v !== null) out.push(v);
    }
    return out;
  }, [answer, lang]);
  const undrawn = answer === null ? 0 : answer.uspace_airspaces.length + answer.zones.length + answer.restrictions.length - zones.length;
  const counts: Partial<Record<ZoneType, number>> = {};
  for (const z of zones) counts[z.type] = (counts[z.type] ?? 0) + 1;

  return (
    <section aria-labelledby="geo-heading" className="flex flex-col gap-3">
      <PageHeading id="geo-heading">{t("portal.geo.heading")}</PageHeading>
      <p className="m-0 text-sm text-[var(--us-text-muted)]">{t(intentId === null ? "portal.geo.note_box" : "portal.geo.note_intent")}</p>
      <RequireSession>
        <IntentPicker path="/geo" current={intentId} />
        {intentId !== null && <GeoChanges intentId={intentId} onChanged={fetchAnswer} />}
        <div className="flex gap-2">
          <Button variant="outline" size="sm" onClick={fetchAnswer}>
            {t("portal.refresh")}
          </Button>
        </div>
        <ProblemNotice error={error} />
        <div className="grid gap-3 lg:grid-cols-[1fr_24rem]">
          <div className="flex flex-col gap-2">
            <PortalMap className="h-[420px] lg:h-[560px]">
              {intentId === null && <ViewBox margin={cfg.geoMarginFraction} onChange={onBox} />}
              <ZoneLayer id="portal-geo" zones={zones} selectedId={selected} onSelect={setSelected} labels />
            </PortalMap>
            {undrawn > 0 && <p role="status">{t("portal.geo.undrawn", { n: undrawn })}</p>}
            <ZoneLegend counts={counts} />
          </div>
          <div className="max-h-[80vh] overflow-y-auto" data-testid="geo-answer">
            {answer === null ? <p role="status">{t("portal.loading")}</p> : <AnswerView a={answer} />}
          </div>
        </div>
      </RequireSession>
    </section>
  );
}
