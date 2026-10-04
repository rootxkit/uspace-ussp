"use client";

// The operator's open intents to follow (GET /v1/intents): the traffic,
// alerts and geo pages are for one intent at a time, named in the URL.
import { useEffect, useState } from "react";
import Link from "next/link";
import { ProblemNotice } from "@/components/ProblemNotice";
import { useAppT } from "@/i18n/t";
import { useApi, type Schemas } from "@/lib/api";

/** The states traffic-ws holds an intent in (intent_active): the others have no stream. */
const FOLLOWED = new Set(["accepted", "activated", "nonconforming", "contingent"]);

export function IntentPicker({ path, current }: { path: string; current: string | null }) {
  const t = useAppT();
  const api = useApi();
  const [list, setList] = useState<Omit<Schemas["IntentDecision"], "alternative">[] | null>(null);
  const [error, setError] = useState<unknown>(null);
  useEffect(() => {
    let live = true;
    api
      .GET("/v1/intents", {})
      .then(({ data }) => live && setList((data?.intents ?? []).filter((d) => FOLLOWED.has(d.state))))
      .catch((e: unknown) => live && setError(e));
    return () => {
      live = false;
    };
  }, [api]);
  if (error !== null) return <ProblemNotice error={error} />;
  if (list === null) return <p role="status">{t("portal.loading")}</p>;
  return (
    <nav aria-label={t("portal.picker.label")} className="flex flex-col gap-1 text-sm" data-testid="intent-picker">
      {list.length === 0 ? (
        <p className="m-0" role="status">
          {t("portal.picker.none")}
        </p>
      ) : (
        <ul className="m-0 flex flex-wrap gap-x-4 gap-y-1 p-0 list-none">
          {list.map((d) => (
            <li key={d.intent_id}>
              <Link href={`${path}?intent=${d.intent_id}`} aria-current={d.intent_id === current ? "page" : undefined} className={d.intent_id === current ? "font-semibold underline" : "underline"}>
                {t("portal.picker.item", { id: d.intent_id.slice(0, 8), state: t(`portal.state.${d.state}`), number: d.authorisation_number ?? "" })}
              </Link>
            </li>
          ))}
        </ul>
      )}
      {current === null && list.length > 0 && <p className="m-0">{t("portal.picker.choose")}</p>}
    </nav>
  );
}
