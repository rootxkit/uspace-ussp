import { en, type MessageKey } from "./en";
import { ka } from "./ka";

export const locales = ["ka", "en"] as const;
export type Locale = (typeof locales)[number];

const catalogues = { en, ka } as const;

export function isLocale(value: unknown): value is Locale {
  return typeof value === "string" && (locales as readonly string[]).includes(value);
}

// pickLocale: an explicit ?lang= wins, then the first supported
// language of Accept-Language, then Georgian.
export function pickLocale(lang: string | string[] | undefined, acceptLanguage: string | null): Locale {
  if (isLocale(lang)) return lang;
  for (const part of (acceptLanguage ?? "").split(",")) {
    const tag = part.split(";")[0].trim().toLowerCase().split("-")[0];
    if (isLocale(tag)) return tag;
  }
  return "ka";
}

export type Translate = (key: MessageKey, vars?: Record<string, string | number>) => string;

export function translator(locale: Locale): Translate {
  const catalogue = catalogues[locale];
  return (key, vars) =>
    catalogue[key].replace(/\{(\w+)\}/g, (match, name: string) => (vars && name in vars ? String(vars[name]) : match));
}
