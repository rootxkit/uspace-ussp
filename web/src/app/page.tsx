import { headers } from "next/headers";
import Link from "next/link";

import { locales, pickLocale, translator, type Translate } from "@/i18n";
import type { MessageKey } from "@/i18n/en";
import { readReadiness } from "@/lib/readiness";

// Rendered per request: the readiness shown is the one read now.
export const dynamic = "force-dynamic";

const stateClass: Record<string, string> = {
  up: "text-green-700 dark:text-green-400",
  degraded: "text-amber-700 dark:text-amber-400",
  down: "text-red-700 dark:text-red-400",
  unknown: "text-red-700 dark:text-red-400",
};

const statusState: Record<string, string> = { ready: "up", degraded: "degraded", not_ready: "down" };

function age(t: Translate, ageS: number | undefined): string {
  return ageS === undefined ? t("readiness.age.never") : t("readiness.age.seconds", { seconds: ageS });
}

export default async function Home({ searchParams }: PageProps<"/">) {
  const locale = pickLocale((await searchParams).lang, (await headers()).get("accept-language"));
  const t = translator(locale);
  const readiness = await readReadiness();

  return (
    <main lang={locale} className="mx-auto w-full max-w-4xl flex-1 p-6 font-sans">
      <header className="mb-6 flex items-start justify-between gap-4">
        <div>
          <h1 className="text-2xl font-bold">{t("app.title")}</h1>
          <p className="text-sm opacity-80">{t("app.subtitle")}</p>
        </div>
        <nav aria-label="language" className="flex gap-3 text-sm">
          {locales.map((l) => (
            <Link
              key={l}
              href={`/?lang=${l}`}
              hrefLang={l}
              aria-current={l === locale ? "true" : undefined}
              className={l === locale ? "font-bold underline" : "underline-offset-2 hover:underline"}
            >
              {t(`lang.${l}`)}
            </Link>
          ))}
        </nav>
      </header>

      <section aria-labelledby="readiness-heading">
        <h2 id="readiness-heading" className="mb-2 text-xl font-bold">
          {t("readiness.heading")}
        </h2>
        {!readiness.reachable ? (
          <div role="alert" data-testid="readiness-unreachable" className="rounded border border-red-600 p-4">
            <p className="font-bold text-red-700 dark:text-red-400">
              {t("readiness.unreachable", { error: readiness.error })}
            </p>
            <p className="text-sm">{t("readiness.unreachable.hint")}</p>
          </div>
        ) : (
          <>
            <p className="mb-2 text-sm opacity-80">{t("readiness.source", { time: readiness.body.checked_at })}</p>
            <p data-testid="readiness-status" className={`mb-4 font-bold ${stateClass[statusState[readiness.body.status]]}`}>
              {t(`readiness.status.${readiness.body.status}` as MessageKey)} (HTTP {readiness.httpStatus})
            </p>
            <table className="w-full border-collapse text-left text-sm">
              <thead>
                <tr className="border-b">
                  <th className="py-1 pr-3">{t("readiness.column.dependency")}</th>
                  <th className="py-1 pr-3">{t("readiness.column.state")}</th>
                  <th className="py-1 pr-3">{t("readiness.column.required")}</th>
                  <th className="py-1 pr-3">{t("readiness.column.since")}</th>
                  <th className="py-1 pr-3">{t("readiness.column.age")}</th>
                  <th className="py-1">{t("readiness.column.detail")}</th>
                </tr>
              </thead>
              <tbody>
                {Object.entries(readiness.body.dependencies)
                  .sort(([a], [b]) => a.localeCompare(b))
                  .map(([name, d]) => (
                    <tr key={name} className="border-b align-top" data-testid={`dependency-${name}`}>
                      <td className="py-1 pr-3 font-mono">{name}</td>
                      <td className={`py-1 pr-3 font-bold ${stateClass[d.state]}`}>
                        {t(`readiness.state.${d.state}` as MessageKey)}
                      </td>
                      <td className="py-1 pr-3">{t(d.required ? "readiness.required.yes" : "readiness.required.no")}</td>
                      <td className="py-1 pr-3 font-mono">{d.since}</td>
                      <td className="py-1 pr-3">{age(t, d.age_s)}</td>
                      <td className="py-1 break-all">{d.detail ?? ""}</td>
                    </tr>
                  ))}
              </tbody>
            </table>
          </>
        )}
      </section>
    </main>
  );
}
