"use client";

// The persistent inputs strip of every console page (brief WP-18; spec
// 05 §6, LESSONS B-11, SC-22): api's link to the bus, the monitor (a
// monitor that stopped writing its status says "conformance and traffic
// alerts stopped since T"), and every source type and instance with its
// state and the time since which it is in it: disabled by whom and when,
// healthy, stale, lagging, unreachable, never heard, or unknown while
// the bus is not connected. When the inputs cannot be read at all, the
// strip says since when; it never goes blank.
import Link from "next/link";
import { createContext, useContext, useState, type ReactNode } from "react";
import { useT } from "@rootxkit/uspace-ui/i18n";
import { SOURCE_STATE_KEYS } from "@rootxkit/uspace-ui/status";
import type { components } from "@/api/generated/openapi";
import { useAppT } from "@/i18n/t";
import { useConsoleApi } from "@/lib/consoleApi";
import { Utc } from "@/portal/common";
import { body, usePoll, type Poll } from "./usePoll";

export type Inputs = components["schemas"]["AdminInputs"];
export type Input = components["schemas"]["AdminInput"];

/** How often the strip reads the inputs. Display-only. */
export const INPUTS_PERIOD_MS = 3000;
/** How many inputs the strip lists before "and N more". Display-only. */
const STRIP_MAX = 14;

const InputsContext = createContext<Poll<Inputs> | null>(null);

/** The one inputs read of the page, shared by the strip and the inputs page. */
export function InputsProvider({ children }: { children: ReactNode }) {
  const api = useConsoleApi();
  const poll = usePoll(() => api.GET("/v1/admin/inputs").then(body), INPUTS_PERIOD_MS);
  return <InputsContext.Provider value={poll}>{children}</InputsContext.Provider>;
}

export function useInputs(): Poll<Inputs> {
  const p = useContext(InputsContext);
  if (p === null) throw new Error("useInputs outside InputsProvider");
  return p;
}

/** An input's state in words: the kit's for its states, ours for unknown. */
export function useInputState(): (state: Input["state"]) => string {
  const t = useAppT();
  const kitT = useT();
  return (state) => (state === "unknown" ? t("console.input.unknown") : kitT(SOURCE_STATE_KEYS[state]));
}

export function inputName(i: Input): string {
  return i.source_instance === undefined ? i.source : `${i.source}/${i.source_instance}`;
}

function iso(ms: number | null): string | null {
  return ms === null ? null : new Date(ms).toISOString();
}

export function InputsStrip() {
  const t = useAppT();
  const stateWord = useInputState();
  const { data, error, failingSinceMs, okAtMs } = useInputs();
  // The browser time this strip first rendered: the "since" of a strip that never read.
  const [loadMs] = useState(() => Date.now());
  return (
    <section aria-label={t("console.strip.label")} data-testid="inputs-strip" className="flex flex-col gap-1 border-b border-[var(--us-border)] bg-[var(--us-surface-sunken)] px-4 py-2 text-xs">
      {error !== null && (
        <p role="alert" className="m-0 font-semibold" data-testid="strip-unread">
          {t("console.strip.unread")} <Utc iso={iso(failingSinceMs ?? loadMs)} />
          {okAtMs !== null && (
            <>
              {" "}
              {t("console.strip.last_read")} <Utc iso={iso(okAtMs)} />
            </>
          )}
        </p>
      )}
      {data === null ? (
        error === null && <p className="m-0">{t("portal.loading")}</p>
      ) : (
        <>
          <div className="flex flex-wrap gap-x-4 gap-y-1">
            <span data-testid="strip-bus" data-state={data.bus.state}>
              <span className="font-semibold">{t("console.strip.bus")}</span> {t(`console.bus.${data.bus.state}`)} {t("console.since")} <Utc iso={data.bus.since} />
            </span>
            <span data-testid="strip-monitor" data-state={data.monitor.state} className={data.monitor.state === "up" ? "" : "font-semibold"}>
              <span className="font-semibold">{t("console.strip.monitor")}</span>{" "}
              {data.monitor.state === "down" && data.monitor.last_heard_at !== undefined ? (
                <span data-testid="monitor-down">
                  {t("console.monitor.stopped_since")} <Utc iso={data.monitor.last_heard_at} />
                </span>
              ) : (
                t(`console.monitor.${data.monitor.state}`)
              )}
            </span>
          </div>
          <ul className="m-0 flex list-none flex-wrap gap-x-4 gap-y-1 p-0">
            {data.sources.slice(0, STRIP_MAX).map((i) => (
              <li key={inputName(i)} data-testid="strip-input" data-input={inputName(i)} data-state={i.state}>
                <span className="font-mono">{inputName(i)}</span> {stateWord(i.state)}
                {i.disabled !== undefined && (
                  <>
                    {" "}
                    {t("console.input.disabled_by", { who: i.disabled.by_who })} <Utc iso={i.disabled.at} />
                  </>
                )}
                {i.disabled === undefined && i.since !== undefined && (
                  <>
                    {" "}
                    {t("console.since")} <Utc iso={i.since} />
                  </>
                )}
              </li>
            ))}
            {data.sources.length > STRIP_MAX && (
              <li>
                <Link href="/console/inputs" className="underline">
                  {t("console.strip.more", { n: data.sources.length - STRIP_MAX })}
                </Link>
              </li>
            )}
          </ul>
        </>
      )}
    </section>
  );
}
