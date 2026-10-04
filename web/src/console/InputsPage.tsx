"use client";

// The inputs page (brief WP-18; spec 05 §6, LESSONS B-11, SC-08): every
// source type and instance with its state and time, its counters and
// detail; for an admin the switch of each, with a reason (403 for any
// other role: the API decides, the page only hides the control); the
// switches stored, with who set them, when and why (reversible: switch
// it on again); the monitor's instances with their status line
// (evaluation_period_s, intent_active, CIS version and age, terrain and
// geoid); and the readiness of every dependency of api.
import { useCallback, useState } from "react";
import { RequireRole } from "@rootxkit/uspace-ui/auth/client";
import { Button, Input, Label, Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@rootxkit/uspace-ui/ui";
import { ProblemNotice } from "@/components/ProblemNotice";
import { useAppT } from "@/i18n/t";
import { useConsoleApi } from "@/lib/consoleApi";
import { Num, PageHeading, Utc } from "@/portal/common";
import { inputName, useInputState, useInputs, type Input as InputItem } from "./InputsStrip";
import { body, usePoll } from "./usePoll";

const SOURCES_PERIOD_MS = 5000;
type SwitchType = "operator_ws" | "network_rid" | "direct_rid" | "ansp_feed" | "adsb_rx";

function SwitchControl({ i, onDone }: { i: InputItem; onDone(): void }) {
  const t = useAppT();
  const api = useConsoleApi();
  const [reason, setReason] = useState("");
  const [error, setError] = useState<unknown>(null);
  const off = i.state !== "disabled";
  const id = `switch-reason-${inputName(i)}`;
  return (
    <div className="flex flex-col gap-1" data-testid="switch">
      <Label htmlFor={id}>{t("console.reason")}</Label>
      <Input id={id} value={reason} maxLength={500} onChange={(e) => setReason(e.target.value)} />
      <Button
        size="sm"
        variant={off ? "destructive" : "default"}
        disabled={reason === ""}
        onClick={() => {
          setError(null);
          api
            .POST("/v1/admin/sources", {
              body: {
                source_type: i.source as SwitchType,
                ...(i.source_instance === undefined ? {} : { instance_id: i.source_instance }),
                enabled: !off,
                reason,
              },
            })
            .then(() => {
              setReason("");
              onDone();
            })
            .catch(setError);
        }}
      >
        {off ? t("console.inputs.switch_off") : t("console.inputs.switch_on")}
      </Button>
      <ProblemNotice error={error} testId="switch-problem" />
    </div>
  );
}

export function ConsoleInputsPage() {
  const t = useAppT();
  const api = useConsoleApi();
  const stateWord = useInputState();
  const inputs = useInputs();
  const readSwitches = useCallback(() => api.GET("/v1/admin/sources").then(body), [api]);
  const switches = usePoll(readSwitches, SOURCES_PERIOD_MS);
  const data = inputs.data;
  const changed = () => {
    inputs.reload();
    switches.reload();
  };
  return (
    <section aria-labelledby="inputs-heading" className="flex flex-col gap-3">
      <PageHeading id="inputs-heading">{t("console.inputs.heading")}</PageHeading>
      <p className="m-0 text-sm text-[var(--us-text-muted)]">{t("console.inputs.note")}</p>
      <ProblemNotice error={inputs.error} />
      {data !== null && (
        <>
          <p className="m-0 text-sm">
            {t("console.inputs.checked")} <Utc iso={data.checked_at} />
          </p>
          {data.switches.state === "unavailable" && (
            <p role="alert" className="m-0 text-sm font-semibold" data-testid="switches-unavailable">
              {t("console.inputs.switches_unavailable")}
              {data.switches.detail !== undefined && <span className="block font-normal">{data.switches.detail}</span>}
            </p>
          )}
          <Table data-testid="inputs-table">
            <TableHeader>
              <TableRow>
                <TableHead>{t("console.inputs.input")}</TableHead>
                <TableHead>{t("console.inputs.state")}</TableHead>
                <TableHead>{t("console.inputs.heard")}</TableHead>
                <TableHead>{t("console.inputs.counters")}</TableHead>
                <TableHead>{t("console.actions")}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {data.sources.map((i) => (
                <TableRow key={inputName(i)} data-testid="input-row" data-input={inputName(i)} data-state={i.state}>
                  <TableCell className="font-mono text-xs">{inputName(i)}</TableCell>
                  <TableCell className="text-xs">
                    <p className="m-0 font-semibold">{stateWord(i.state)}</p>
                    {i.disabled !== undefined ? (
                      <p className="m-0" data-testid="disabled-by">
                        {t("console.input.disabled_by", { who: i.disabled.by_who })} <Utc iso={i.disabled.at} /> ({t(`console.input.by_${i.disabled.by}`)}): {i.disabled.reason}
                      </p>
                    ) : (
                      i.since !== undefined && (
                        <p className="m-0">
                          {t("console.since")} <Utc iso={i.since} />
                        </p>
                      )
                    )}
                    {i.lag_s !== undefined && i.lag_s > 0 && (
                      <p className="m-0">
                        {t("console.inputs.lag")} <Num v={i.lag_s} unit="s" />
                      </p>
                    )}
                    {i.detail !== undefined && <p className="m-0">{i.detail}</p>}
                  </TableCell>
                  <TableCell className="text-xs">
                    {i.last_heard_at === undefined ? t("console.inputs.never") : <Utc iso={i.last_heard_at} />}
                    {i.age_s !== undefined && (
                      <p className="m-0">
                        {t("console.inputs.age")} <Num v={i.age_s} unit="s" />
                      </p>
                    )}
                  </TableCell>
                  <TableCell className="font-mono text-xs">
                    {i.counters === undefined
                      ? ""
                      : Object.entries(i.counters)
                          .map(([k, v]) => `${k} ${v}`)
                          .join(", ")}
                  </TableCell>
                  <TableCell className="text-xs">
                    <RequireRole anyOf={["admin"]} fallback={t("console.read_only")}>
                      <SwitchControl i={i} onDone={changed} />
                    </RequireRole>
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
          {data.sources_truncated === true && <p className="m-0">{t("portal.list.truncated")}</p>}
          <section aria-labelledby="monitor-heading" data-testid="monitor" data-state={data.monitor.state}>
            <h2 id="monitor-heading" className="m-0 mb-1 text-base font-semibold">
              {t("console.strip.monitor")}: {t(`console.monitor.${data.monitor.state}`)}
            </h2>
            <p className="m-0 text-sm">{data.monitor.detail}</p>
            {data.monitor.instances.length > 0 && (
              <Table>
                <TableHeader>
                  <TableRow>
                    <TableHead>{t("console.monitor.instance")}</TableHead>
                    <TableHead>{t("console.monitor.written")}</TableHead>
                    <TableHead>{t("console.monitor.evaluation")}</TableHead>
                    <TableHead>{t("console.monitor.flights")}</TableHead>
                    <TableHead>{t("console.monitor.cis")}</TableHead>
                    <TableHead>{t("console.monitor.grids")}</TableHead>
                  </TableRow>
                </TableHeader>
                <TableBody>
                  {data.monitor.instances.map((m) => (
                    <TableRow key={m.instance} data-testid="monitor-instance">
                      <TableCell className="font-mono text-xs">{m.instance}</TableCell>
                      <TableCell className="text-xs">
                        <Utc iso={m.at} /> (<Num v={m.age_s} unit="s" />)
                      </TableCell>
                      <TableCell className="text-xs">
                        <Num v={m.evaluation_period_s} digits={2} unit="s" /> · CPA <Num v={m.cpa_evaluation_period_s} digits={2} unit="s" />
                      </TableCell>
                      <TableCell className="text-xs">
                        {m.flights_tracked} · {t("console.monitor.workers", { n: m.workers })}
                        {m.intent_active_age_s === undefined ? <p className="m-0 font-semibold">{t("console.monitor.intents_unread")}</p> : null}
                      </TableCell>
                      <TableCell className="text-xs">
                        {m.cis_loaded ? m.cis_version : t("console.monitor.cis_unloaded")} (<Num v={m.cis_age_s} digits={0} unit="s" />)
                        {m.cis_stale === true && <p className="m-0 font-semibold">{t("console.monitor.cis_stale")}</p>}
                      </TableCell>
                      <TableCell className="text-xs">
                        {t("console.monitor.terrain")} {m.terrain ? t("portal.yes") : t("portal.no")} · {t("console.monitor.geoid")} {m.geoid ? t("portal.yes") : t("portal.no")}
                      </TableCell>
                    </TableRow>
                  ))}
                </TableBody>
              </Table>
            )}
          </section>
          <section aria-labelledby="deps-heading">
            <h2 id="deps-heading" className="m-0 mb-1 text-base font-semibold">
              {t("console.inputs.dependencies")}
            </h2>
            <ul className="m-0 flex list-none flex-col gap-1 p-0 text-xs" data-testid="dependencies">
              {Object.entries(data.dependencies)
                .sort(([a], [b]) => a.localeCompare(b))
                .map(([name, d]) => (
                  <li key={name} data-testid="dependency" data-dependency={name} data-state={d.state} className="break-words">
                    <span className="font-mono font-semibold">{name}</span> {t(`console.dep.${d.state}`)} {t("console.since")} <Utc iso={d.since} />
                    {d.detail !== undefined && <span>: {d.detail}</span>}
                  </li>
                ))}
            </ul>
          </section>
        </>
      )}
      <section aria-labelledby="switches-heading">
        <h2 id="switches-heading" className="m-0 mb-1 text-base font-semibold">
          {t("console.inputs.switches")}
        </h2>
        <ProblemNotice error={switches.error} />
        {switches.data !== null && switches.data.switches.length === 0 && <p className="m-0 text-sm">{t("console.inputs.no_switches")}</p>}
        {switches.data !== null && switches.data.switches.length > 0 && (
          <Table data-testid="switches">
            <TableHeader>
              <TableRow>
                <TableHead>{t("console.inputs.input")}</TableHead>
                <TableHead>{t("console.inputs.state")}</TableHead>
                <TableHead>{t("console.audit.actor")}</TableHead>
                <TableHead>{t("console.reason")}</TableHead>
                <TableHead>{t("console.audit.at")}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {switches.data.switches.map((s) => (
                <TableRow key={`${s.source_type}/${s.instance_id ?? ""}`} data-testid="switch-row" data-input={s.instance_id === undefined ? s.source_type : `${s.source_type}/${s.instance_id}`}>
                  <TableCell className="font-mono text-xs">{s.instance_id === undefined ? s.source_type : `${s.source_type}/${s.instance_id}`}</TableCell>
                  <TableCell className="text-xs">{s.enabled ? t("console.inputs.on") : t("console.inputs.off")}</TableCell>
                  <TableCell className="text-xs">{s.actor}</TableCell>
                  <TableCell className="text-xs">{s.reason}</TableCell>
                  <TableCell className="text-xs">
                    <Utc iso={s.changed_at} /> (v{s.version})
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </section>
    </section>
  );
}
