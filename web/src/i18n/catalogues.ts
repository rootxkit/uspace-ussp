// The portal's own ka/en catalogues, handed to the kit's I18nProvider
// (which puts them ahead of the kit's). Every display string comes from
// here or from the kit. ka is typed by en: a key missing in ka fails
// `tsc` and the build; scripts/check-i18n.mjs fails on an extra key, an
// empty value, a placeholder that differs between the two, and any
// word of manoeuvre advice (LESSONS X-15) in either.
import type { Catalogues, Lang } from "@rootxkit/uspace-ui/i18n";
import en from "./en.json";
import kaRaw from "./ka.json";

export type AppKey = keyof typeof en;

const ka: Record<AppKey, string> = kaRaw;

export const catalogues: Catalogues = { ka, en };

export function catalogueOf(lang: Lang): Record<AppKey, string> {
  return lang === "en" ? en : ka;
}
