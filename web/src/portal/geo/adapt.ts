// GeoAnswer items (GET /v1/geo, GET /v1/geo/intents/{id}) onto the kit's
// ZoneView, copying what the API said and deciding nothing: the ED-318
// geometry as served, the limits only when the feature has one part and
// the API gave them in metres with their reference (never converted
// here), applies from the API's at_state one to one (unknown stays
// unknown: null), the restriction's state as the API says it.
import type { Lang } from "@rootxkit/uspace-ui/i18n";
import { isRestrictionState, isZoneType, type ZoneView } from "@rootxkit/uspace-ui/model";
import type { Schemas } from "@/lib/api";

type Item = Schemas["GeoItem"];

type Text = { text?: unknown; lang?: unknown };

function textIn(list: unknown, lang: Lang): string | null {
  if (!Array.isArray(list) || list.length === 0) return null;
  const texts = list.filter((x): x is Text => typeof x === "object" && x !== null);
  const hit = texts.find((x) => typeof x.lang === "string" && x.lang.toLowerCase().startsWith(lang)) ?? texts[0];
  return typeof hit?.text === "string" ? hit.text : null;
}

/** A GeoItem as a ZoneView; null when its feature has no geometry or its type is not one the kit draws (it is then listed, not drawn). */
export function toZoneView(item: Item, lang: Lang, restriction?: Schemas["GeoRestriction"]): ZoneView | null {
  const f = item.feature as { geometry?: unknown; properties?: Record<string, unknown> };
  const p = f.properties ?? {};
  if (typeof f.geometry !== "object" || f.geometry === null) return null;
  if (!isZoneType(item.type)) return null;
  const one = item.parts.length === 1 ? item.parts[0] : undefined;
  const state = restriction?.state ?? null;
  return {
    identifier: item.identifier,
    name: textIn(p["name"], lang),
    type: item.type,
    variant: typeof p["variant"] === "string" ? p["variant"] : null,
    reason: Array.isArray(p["reason"]) ? p["reason"].filter((r): r is string => typeof r === "string") : [],
    message: textIn(p["message"], lang),
    lowerLimitM: one?.lower?.value_m ?? null,
    lowerRef: one?.lower?.ref ?? null,
    upperLimitM: one?.upper?.value_m ?? null,
    upperRef: one?.upper?.ref ?? null,
    geometry: f.geometry as ZoneView["geometry"],
    applies: item.applicability.at_state === "applies" ? true : null,
    restrictionState: state !== null && isRestrictionState(state) ? state : null,
    version: item.version,
    updatedAt: item.updated_at,
  };
}
