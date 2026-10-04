// The request's language and the portal's own strings on the server,
// for what renders outside the providers: the page <title> (WCAG 2.4.2).
import "server-only";
import { cookies, headers } from "next/headers";
import { LANG_COOKIE, interpolate, negotiateLang, type Lang } from "@rootxkit/uspace-ui/i18n";
import { brandFromEnv } from "@rootxkit/uspace-ui/theme";
import { catalogueOf, type AppKey } from "./catalogues";

/** The uspace_lang cookie, then Accept-Language, then ka. */
export async function requestLang(): Promise<Lang> {
  const jar = await cookies();
  const h = await headers();
  return negotiateLang(h.get("accept-language"), jar.get(LANG_COOKIE)?.value ?? null);
}

/** "<page> · <brand>", in the request's language. */
export async function pageTitle(key: AppKey): Promise<string> {
  const cat = catalogueOf(await requestLang());
  return interpolate(cat["portal.page_title"], { page: cat[key], brand: brandFromEnv(process.env).name });
}
