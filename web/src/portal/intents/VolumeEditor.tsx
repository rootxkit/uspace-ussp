"use client";

// One operational volume's outline (Annex IV item 5): a polygon's
// vertices or a circle's centre and radius, entered as numbers or by
// clicking the map, and sent as an ASTM F3548 Volume4D outline exactly as
// entered (brief WP-17). The page does no geometry: no area, no buffer,
// no circle drawn as a polygon, no check that a polygon is simple. The
// API judges the outline and returns the AMSL derivation and every
// problem by its field path.
//
// The map shows the points as clicked or typed and the polygon's edges
// in the order given (the first vertex repeated to close the line is a
// copy, not a computation). A circle is shown as its centre with its
// radius in words: drawing it would need geometry in the browser.
import { useCallback, useEffect, useId, useState } from "react";
import type { GeoJSONSource, Map as MapLibreMap, MapMouseEvent } from "maplibre-gl";
import { useLayer, resolveColour } from "@rootxkit/uspace-ui/layers";
import { useMap } from "@rootxkit/uspace-ui/map";
import { formatLocaleNumber, parseLocaleNumber } from "@rootxkit/uspace-ui/form";
import { useLang } from "@rootxkit/uspace-ui/i18n";
import { Button, Input, Label } from "@rootxkit/uspace-ui/ui";
import { useAppT } from "@/i18n/t";
import type { Schemas } from "@/lib/api";

export type Point = Schemas["IntentPoint"];

export type Outline =
  | { kind: "polygon"; vertices: Point[] }
  | { kind: "circle"; center: Point | null; radiusM: number | null };

/**
 * The most vertices the editor takes; the API bounds a volume's outline
 * too and says so by field. A display bound, not a judgement.
 */
export const MAX_VERTICES = 100;

/** The F3548 outline member of a Volume3D, as entered; null while it is incomplete. */
export function outlineOf(o: Outline): Schemas["IntentVolume4D"]["volume"] | null {
  if (o.kind === "polygon") {
    return o.vertices.length === 0 ? null : ({ outline_polygon: { vertices: o.vertices } } as Schemas["IntentVolume4D"]["volume"]);
  }
  if (o.center === null || o.radiusM === null) return null;
  return { outline_circle: { center: o.center, radius: { value: o.radiusM, units: "M" } } } as Schemas["IntentVolume4D"]["volume"];
}

const DRAFT = "portal-draft";

/** The draft outline on the map: points and, for a polygon, its edges. */
export function DraftLayer({ outline, onPick }: { outline: Outline; onPick(p: Point): void }) {
  const map = useMap();
  const points: Point[] = outline.kind === "polygon" ? outline.vertices : outline.center === null ? [] : [outline.center];
  const data: GeoJSON.FeatureCollection = {
    type: "FeatureCollection",
    features: [
      ...points.map((p, i) => ({
        type: "Feature" as const,
        properties: { label: String(i + 1) },
        geometry: { type: "Point" as const, coordinates: [p.lng, p.lat] },
      })),
      ...(outline.kind === "polygon" && outline.vertices.length > 1
        ? [
            {
              type: "Feature" as const,
              properties: {},
              geometry: {
                type: "LineString" as const,
                coordinates: [...outline.vertices, outline.vertices[0] as Point].map((p) => [p.lng, p.lat]),
              },
            },
          ]
        : []),
    ],
  };
  useLayer<GeoJSON.FeatureCollection>({
    id: DRAFT,
    data,
    build: (m: MapLibreMap) => {
      const colour = resolveColour(m, "--us-brand-accent");
      m.addSource(DRAFT, { type: "geojson", data: { type: "FeatureCollection", features: [] } });
      m.addLayer({ id: `${DRAFT}-line`, type: "line", source: DRAFT, filter: ["==", ["geometry-type"], "LineString"], paint: { "line-color": colour, "line-width": 2, "line-dasharray": [2, 1] } });
      m.addLayer({ id: `${DRAFT}-points`, type: "circle", source: DRAFT, filter: ["==", ["geometry-type"], "Point"], paint: { "circle-color": colour, "circle-radius": 5 } });
      return [`${DRAFT}-line`, `${DRAFT}-points`];
    },
    update: (m: MapLibreMap, d: GeoJSON.FeatureCollection) => {
      (m.getSource(DRAFT) as GeoJSONSource | undefined)?.setData(d);
    },
  });
  const onClick = useCallback((e: MapMouseEvent) => onPick({ lat: e.lngLat.lat, lng: e.lngLat.lng }), [onPick]);
  // The map's click is a point as MapLibre reports it: no snapping, no rounding.
  useClick(map, onClick);
  return null;
}

function useClick(map: MapLibreMap | null, fn: (e: MapMouseEvent) => void) {
  useEffect(() => {
    if (map === null) return;
    map.on("click", fn);
    return () => {
      map.off("click", fn);
    };
  }, [map, fn]);
}

function NumberBox({ id, label, value, onChange }: { id: string; label: string; value: number | null; onChange(v: number | null): void }) {
  const { lang } = useLang();
  const [text, setText] = useState(formatLocaleNumber(value, lang));
  return (
    <div className="flex flex-col gap-1">
      <Label htmlFor={id}>{label}</Label>
      <Input
        id={id}
        inputMode="decimal"
        autoComplete="off"
        value={text}
        onChange={(e) => {
          setText(e.target.value);
          onChange(parseLocaleNumber(e.target.value, lang));
        }}
      />
    </div>
  );
}

/** The outline's fields: the kind, the vertices or the circle, and the way to clear them. */
export function OutlineFields({ outline, onChange }: { outline: Outline; onChange(o: Outline): void }) {
  const t = useAppT();
  const { lang } = useLang();
  const base = useId();
  const [lat, setLat] = useState<number | null>(null);
  const [lng, setLng] = useState<number | null>(null);
  const [key, setKey] = useState(0);
  const point = lat !== null && lng !== null ? { lat, lng } : null;
  return (
    <fieldset className="flex flex-col gap-2 rounded-md border border-[var(--us-border)] p-3" data-testid="outline">
      <legend className="px-1 text-sm font-medium">{t("portal.intent.outline.legend")}</legend>
      <p className="m-0 text-xs text-[var(--us-text-muted)]">{t("portal.intent.outline.hint")}</p>
      <div role="radiogroup" aria-label={t("portal.intent.outline.kind")} className="flex gap-4 text-sm">
        {(["polygon", "circle"] as const).map((k) => (
          <label key={k} className="flex items-center gap-1">
            <input
              type="radio"
              name={`${base}-kind`}
              checked={outline.kind === k}
              onChange={() => onChange(k === "polygon" ? { kind: "polygon", vertices: [] } : { kind: "circle", center: null, radiusM: null })}
            />
            {t(k === "polygon" ? "portal.intent.outline.polygon" : "portal.intent.outline.circle")}
          </label>
        ))}
      </div>
      <div key={key} className="grid gap-2 sm:grid-cols-3">
        <NumberBox id={`${base}-lat`} label={t("portal.intent.outline.lat")} value={lat} onChange={setLat} />
        <NumberBox id={`${base}-lng`} label={t("portal.intent.outline.lng")} value={lng} onChange={setLng} />
        <Button
          type="button"
          variant="outline"
          className="self-end"
          disabled={point === null || (outline.kind === "polygon" && outline.vertices.length >= MAX_VERTICES)}
          onClick={() => {
            if (point === null) return;
            onChange(outline.kind === "polygon" ? { ...outline, vertices: [...outline.vertices, point] } : { ...outline, center: point });
            setLat(null);
            setLng(null);
            setKey((k) => k + 1);
          }}
        >
          {t(outline.kind === "polygon" ? "portal.intent.outline.add_vertex" : "portal.intent.outline.set_center")}
        </Button>
      </div>
      {outline.kind === "circle" && (
        <NumberBox
          id={`${base}-radius`}
          label={t("portal.intent.outline.radius")}
          value={outline.radiusM}
          onChange={(v) => onChange({ ...outline, radiusM: v })}
        />
      )}
      <ol className="m-0 flex flex-col gap-1 pl-5 text-sm" aria-label={t("portal.intent.outline.points")} data-testid="outline-points">
        {(outline.kind === "polygon" ? outline.vertices : outline.center === null ? [] : [outline.center]).map((p, i) => (
          <li key={`${i}-${p.lat}-${p.lng}`}>
            {t("portal.intent.outline.point", { lat: formatLocaleNumber(p.lat, lang), lng: formatLocaleNumber(p.lng, lang) })}
          </li>
        ))}
      </ol>
      {outline.kind === "circle" && outline.center !== null && outline.radiusM !== null && (
        <p className="m-0 text-xs">{t("portal.intent.outline.circle_note", { r: formatLocaleNumber(outline.radiusM, lang) })}</p>
      )}
      <Button
        type="button"
        variant="outline"
        size="sm"
        className="self-start"
        onClick={() => onChange(outline.kind === "polygon" ? { kind: "polygon", vertices: [] } : { kind: "circle", center: null, radiusM: null })}
      >
        {t("portal.intent.outline.clear")}
      </Button>
    </fieldset>
  );
}
