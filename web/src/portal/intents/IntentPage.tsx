"use client";

// One intent's decision as it stands (GET /v1/intents/{id};
// intent/decision/v1): the decision and state, the authorisation number,
// the deviation thresholds, every conflict with the Annex IV item, zone,
// airspace, restriction, intent or registry key it names, the
// conditions, the AMSL derivation of each volume, the versions it rests
// on (CIS, registry, policy, weather) and its version with the change
// reason (brief WP-17). Every number here is a response field; the page
// computes none. An operator_admin or a remote_pilot activates, modifies
// (a new version, decided again) or ends it.
import { useCallback, useEffect, useState } from "react";
import Link from "next/link";
import { z } from "zod";
import { RequireRole } from "@rootxkit/uspace-ui/auth/client";
import { Form, NumberField, TextField, UTCDateTimeField } from "@rootxkit/uspace-ui/form";
import type { FieldError } from "@rootxkit/uspace-ui/model";
import { Button, Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from "@rootxkit/uspace-ui/ui";
import { ProblemNotice } from "@/components/ProblemNotice";
import { catalogueOf, type AppKey } from "@/i18n/catalogues";
import { useAppT } from "@/i18n/t";
import { useApi, type Schemas } from "@/lib/api";
import { Fact, Num, PageHeading, RequireSession, Utc } from "../common";
import { PortalMap } from "../PortalMap";
import { DraftLayer, OutlineFields, outlineOf, type Outline, type Point } from "./VolumeEditor";

// The answer as openapi-fetch types it: alternative (always null in v1) is not on it.
type Decision = Omit<Schemas["IntentDecision"], "alternative">;

/** The states an activation and an end may start from: the API answers 409 for any other; this only hides the buttons. */
const ACTIVATABLE = new Set(["accepted"]);
const ENDABLE = new Set(["pending_validation", "pending_dss", "pending_authority", "accepted", "activated", "nonconforming", "contingent"]);

function itemName(t: (k: AppKey) => string, item: number | null): string {
  if (item === null) return t("portal.decision.no_item");
  return item >= 1 && item <= 10 ? `${item}. ${t(`portal.item.${item}` as AppKey)}` : String(item);
}

/**
 * A condition in the reader's language (portal.decision.condition.<code>;
 * scripts/check-i18n.mjs fails when a code of internal/intent has no
 * key). A code the catalogues do not name (the list is open) is shown as
 * sent.
 */
function conditionName(t: (k: AppKey) => string, code: string): string {
  const key = `portal.decision.condition.${code}`;
  return key in catalogueOf("en") ? t(key as AppKey) : code;
}

function Conflicts({ d }: { d: Decision }) {
  const t = useAppT();
  if (d.conflicts.length === 0) {
    return (
      <p role="status" data-testid="no-conflicts">
        {t("portal.decision.no_conflicts")}
      </p>
    );
  }
  return (
    <Table data-testid="conflicts">
      <TableHeader>
        <TableRow>
          <TableHead>{t("portal.decision.conflict.kind")}</TableHead>
          <TableHead>{t("portal.decision.conflict.reason")}</TableHead>
          <TableHead>{t("portal.decision.conflict.effect")}</TableHead>
          <TableHead>{t("portal.decision.conflict.item")}</TableHead>
          <TableHead>{t("portal.decision.conflict.ref")}</TableHead>
          <TableHead>{t("portal.decision.conflict.volume")}</TableHead>
          <TableHead>{t("portal.decision.conflict.overlap")}</TableHead>
          <TableHead>{t("portal.decision.conflict.detail")}</TableHead>
        </TableRow>
      </TableHeader>
      <TableBody>
        {d.conflicts.map((c, i) => (
          <TableRow key={`${c.kind}-${c.reason}-${i}`} data-testid="conflict" data-reason={c.reason}>
            <TableCell>{t(`portal.decision.kind.${c.kind}`)}</TableCell>
            <TableCell className="font-mono text-xs">{c.reason}</TableCell>
            <TableCell>{t(`portal.decision.effect.${c.effect}`)}</TableCell>
            <TableCell>{itemName(t, c.item)}</TableCell>
            <TableCell className="font-mono text-xs">{c.ref ?? ""}</TableCell>
            <TableCell>{c.volume === null ? "" : c.volume + 1}</TableCell>
            <TableCell>
              {c.overlap === null ? (
                ""
              ) : (
                <>
                  <Num v={c.overlap.h_m} unit="m" /> / <Num v={c.overlap.v_m} unit="m" /> / <Num v={c.overlap.t_s} unit="s" />
                </>
              )}
            </TableCell>
            <TableCell>{c.detail}</TableCell>
          </TableRow>
        ))}
      </TableBody>
    </Table>
  );
}

function DecisionView({ d }: { d: Decision }) {
  const t = useAppT();
  return (
    <div className="flex flex-col gap-3">
      <section aria-labelledby="decision-heading" data-testid="decision" data-decision={d.decision} data-state={d.state} className="rounded border border-[var(--us-border)] p-3">
        <h2 id="decision-heading" className="m-0 text-base font-semibold">
          {t(`portal.decision.decision.${d.decision}`)}
        </h2>
        <dl className="m-0 mt-2 grid gap-2 sm:grid-cols-3">
          <Fact term={t("portal.decision.state")} testId="state">
            {t(`portal.state.${d.state}`)}
          </Fact>
          <Fact term={t("portal.decision.dss_state")}>{d.dss_state ?? t("portal.decision.dss_none")}</Fact>
          <Fact term={t("portal.decision.authorisation_number")} testId="authorisation-number">
            {d.authorisation_number ?? t("portal.decision.no_number")}
          </Fact>
          <Fact term={t("portal.decision.valid_from")}>
            <Utc iso={d.valid_from} />
          </Fact>
          <Fact term={t("portal.decision.valid_to")}>
            <Utc iso={d.valid_to} />
          </Fact>
          <Fact term={t("portal.decision.priority")}>{d.priority}</Fact>
          <Fact term={t("portal.decision.in_uspace")}>
            {t(d.in_uspace_airspace ? "portal.yes" : "portal.no")}
            {d.uspace_airspace_ids.length > 0 && ` (${d.uspace_airspace_ids.join(", ")})`}
          </Fact>
          <Fact term={t("portal.decision.exempt")}>{t(d.exempt_art_1_3 ? "portal.yes" : "portal.no")}</Fact>
          <Fact term={t("portal.decision.version")} testId="version">
            {d.version}
          </Fact>
          <Fact term={t("portal.decision.change_reason")}>{d.change_reason ?? ""}</Fact>
          <Fact term={t("portal.decision.decided_at")}>
            <Utc iso={d.decided_at} />
          </Fact>
          <Fact term={t("portal.decision.updated_at")}>
            <Utc iso={d.updated_at} />
          </Fact>
        </dl>
      </section>
      <section aria-labelledby="thresholds-heading">
        <h2 id="thresholds-heading" className="m-0 mb-1 text-base font-semibold">
          {t("portal.decision.thresholds")}
        </h2>
        {d.deviation_thresholds === null ? (
          <p className="m-0">{t("portal.decision.no_thresholds")}</p>
        ) : (
          <dl className="m-0 grid gap-2 sm:grid-cols-3" data-testid="thresholds">
            <Fact term={t("portal.decision.threshold_h")}>
              <Num v={d.deviation_thresholds.h_m} unit="m" />
            </Fact>
            <Fact term={t("portal.decision.threshold_v")}>
              <Num v={d.deviation_thresholds.v_m} unit="m" />
            </Fact>
            <Fact term={t("portal.decision.threshold_t")}>
              <Num v={d.deviation_thresholds.t_s} unit="s" />
            </Fact>
          </dl>
        )}
      </section>
      <section aria-labelledby="conflicts-heading">
        <h2 id="conflicts-heading" className="m-0 mb-1 text-base font-semibold">
          {t("portal.decision.conflicts", { n: d.conflicts.length })}
        </h2>
        <Conflicts d={d} />
      </section>
      <section aria-labelledby="conditions-heading">
        <h2 id="conditions-heading" className="m-0 mb-1 text-base font-semibold">
          {t("portal.decision.conditions", { n: d.conditions.length })}
        </h2>
        <ul className="m-0 flex flex-col gap-1 pl-5" data-testid="conditions">
          {d.conditions.map((c, i) => (
            <li key={`${c.code}-${i}`} data-testid="condition" data-code={c.code}>
              <span>{conditionName(t, c.code)}</span> <span className="font-mono text-xs">{c.code}</span>
              {c.ref !== undefined && <span className="font-mono text-xs"> ({c.ref})</span>}: {c.detail}
            </li>
          ))}
        </ul>
      </section>
      <section aria-labelledby="amsl-heading">
        <h2 id="amsl-heading" className="m-0 mb-1 text-base font-semibold">
          {t("portal.decision.volumes_amsl")}
        </h2>
        <Table data-testid="volumes-amsl">
          <TableHeader>
            <TableRow>
              <TableHead>{t("portal.decision.volume")}</TableHead>
              <TableHead>{t("portal.decision.lower_w84")}</TableHead>
              <TableHead>{t("portal.decision.upper_w84")}</TableHead>
              <TableHead>{t("portal.decision.undulation")}</TableHead>
              <TableHead>{t("portal.decision.lower_amsl")}</TableHead>
              <TableHead>{t("portal.decision.upper_amsl")}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {d.volumes_amsl.map((v, i) => (
              <TableRow key={i}>
                <TableCell>{i + 1}</TableCell>
                <TableCell>
                  <Num v={v.lower_w84_m} unit="m" />
                </TableCell>
                <TableCell>
                  <Num v={v.upper_w84_m} unit="m" />
                </TableCell>
                <TableCell>
                  <Num v={v.undulation_m} unit="m" />
                </TableCell>
                <TableCell>
                  <Num v={v.lower_amsl_m} unit="m" />
                </TableCell>
                <TableCell>
                  <Num v={v.upper_amsl_m} unit="m" />
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </section>
      <section aria-labelledby="basis-heading">
        <h2 id="basis-heading" className="m-0 mb-1 text-base font-semibold">
          {t("portal.decision.basis")}
        </h2>
        <dl className="m-0 grid gap-2 sm:grid-cols-3">
          <Fact term={t("portal.decision.cis_version")}>{d.cis_version_checked ?? t("portal.decision.cis_not_consulted")}</Fact>
          <Fact term={t("portal.decision.cis_age")}>
            <Num v={d.cis_age_s} unit="s" />
          </Fact>
          <Fact term={t("portal.decision.registry_checked_at")}>
            <Utc iso={d.registry_checked_at} />
          </Fact>
          <Fact term={t("portal.decision.policy_version")}>{d.policy_version}</Fact>
          <Fact term={t("portal.decision.weather_ref")}>{d.weather_checked_ref ?? t("portal.decision.weather_none")}</Fact>
          <Fact term={t("portal.decision.client_ref")}>{d.client_ref}</Fact>
        </dl>
      </section>
    </div>
  );
}

const modifySchema = z.object({
  volumes: z
    .array(
      z.object({
        volume: z.object({ altitude_lower: z.object({ value: z.number() }), altitude_upper: z.object({ value: z.number() }) }),
        time_start: z.object({ value: z.string() }),
        time_end: z.object({ value: z.string() }),
      }),
    )
    .length(1),
  change_reason: z.string().trim().min(1).max(256),
});

function ModifyForm({ id, onDone }: { id: string; onDone(d: Decision): void }) {
  const t = useAppT();
  const api = useApi();
  const [outline, setOutline] = useState<Outline>({ kind: "polygon", vertices: [] });
  const onPick = useCallback(
    (p: Point) => setOutline((o) => (o.kind === "polygon" ? { ...o, vertices: [...o.vertices, p] } : { ...o, center: p })),
    [],
  );
  return (
    <section aria-labelledby="modify-heading" className="flex flex-col gap-2 rounded border border-[var(--us-border)] p-3">
      <h2 id="modify-heading" className="m-0 text-base font-semibold">
        {t("portal.intent.modify_heading")}
      </h2>
      <p className="m-0 text-xs text-[var(--us-text-muted)]">{t("portal.intent.modify_note")}</p>
      <div className="grid gap-4 lg:grid-cols-2">
        <div className="flex flex-col gap-2">
          <PortalMap className="h-[300px]">
            <DraftLayer outline={outline} onPick={onPick} />
          </PortalMap>
          <OutlineFields outline={outline} onChange={setOutline} />
        </div>
        <Form
          schema={modifySchema}
          defaults={{
            volumes: [
              {
                volume: { altitude_lower: { value: null as unknown as number }, altitude_upper: { value: null as unknown as number } },
                time_start: { value: null as unknown as string },
                time_end: { value: null as unknown as string },
              },
            ],
            change_reason: "",
          }}
          submitLabelKey="portal.intent.modify"
          onSubmit={async (v): Promise<FieldError[] | undefined> => {
            const o = outlineOf(outline);
            const vol = v.volumes[0];
            if (o === null || vol === undefined) return [{ field: "volumes[0].volume", reason: t("portal.intent.outline.missing") }];
            const { data } = await api.PATCH("/v1/intents/{intent_id}", {
              params: { path: { intent_id: id } },
              body: {
                action: "modify",
                change_reason: v.change_reason,
                volumes: [
                  {
                    volume: {
                      ...o,
                      altitude_lower: { value: vol.volume.altitude_lower.value, reference: "W84", units: "M" },
                      altitude_upper: { value: vol.volume.altitude_upper.value, reference: "W84", units: "M" },
                    },
                    time_start: { value: vol.time_start.value, format: "RFC3339" },
                    time_end: { value: vol.time_end.value, format: "RFC3339" },
                  },
                ],
              },
            });
            if (data !== undefined) onDone(data);
            return undefined;
          }}
        >
          <NumberField name="volumes.0.volume.altitude_lower.value" labelKey="portal.intent.alt_lower" unit="form.unit.m" datum="WGS84" required />
          <NumberField name="volumes.0.volume.altitude_upper.value" labelKey="portal.intent.alt_upper" unit="form.unit.m" datum="WGS84" required />
          <UTCDateTimeField name="volumes.0.time_start.value" labelKey="portal.intent.time_start" required />
          <UTCDateTimeField name="volumes.0.time_end.value" labelKey="portal.intent.time_end" required />
          <TextField name="change_reason" labelKey="portal.intent.change_reason" required />
        </Form>
      </div>
    </section>
  );
}

export function IntentPage({ id }: { id: string }) {
  const t = useAppT();
  const api = useApi();
  const [d, setD] = useState<Decision | null>(null);
  const [error, setError] = useState<unknown>(null);
  const [actionError, setActionError] = useState<unknown>(null);
  const [busy, setBusy] = useState(false);
  const [modifying, setModifying] = useState(false);
  const load = useCallback(() => {
    api
      .GET("/v1/intents/{intent_id}", { params: { path: { intent_id: id } } })
      .then(({ data }) => {
        setD(data ?? null);
        setError(null);
      })
      .catch(setError);
  }, [api, id]);
  useEffect(load, [load]);
  const act = (action: "activate" | "end") => {
    setBusy(true);
    setActionError(null);
    api
      .PATCH("/v1/intents/{intent_id}", { params: { path: { intent_id: id } }, body: { action } })
      .then(({ data }) => setD(data ?? null))
      .catch(setActionError)
      .finally(() => setBusy(false));
  };
  return (
    <section aria-labelledby="intent-heading" className="flex flex-col gap-3">
      <PageHeading id="intent-heading">{t("portal.intent.heading", { id })}</PageHeading>
      <RequireSession>
        <ProblemNotice error={error} />
        {d === null && error === null && <p role="status">{t("portal.loading")}</p>}
        {d !== null && (
          <>
            <nav aria-label={t("portal.intent.links")} className="flex flex-wrap gap-4 text-sm">
              <Link href={`/geo?intent=${id}`} className="underline">
                {t("portal.intent.open_geo")}
              </Link>
              <Link href={`/traffic?intent=${id}`} className="underline">
                {t("portal.intent.open_traffic")}
              </Link>
              <Link href={`/alerts?intent=${id}`} className="underline">
                {t("portal.intent.open_alerts")}
              </Link>
            </nav>
            <RequireRole anyOf={["operator_admin", "remote_pilot"]}>
              <div className="flex flex-wrap gap-2" role="group" aria-label={t("portal.intent.actions")}>
                {ACTIVATABLE.has(d.state) && (
                  <Button disabled={busy} onClick={() => act("activate")}>
                    {t("portal.intent.activate")}
                  </Button>
                )}
                {d.state === "accepted" && (
                  <Button variant="outline" disabled={busy} onClick={() => setModifying((m) => !m)}>
                    {t("portal.intent.modify")}
                  </Button>
                )}
                {ENDABLE.has(d.state) && (
                  <Button variant="outline" disabled={busy} onClick={() => act("end")}>
                    {t("portal.intent.end")}
                  </Button>
                )}
                <Button variant="outline" onClick={load}>
                  {t("portal.refresh")}
                </Button>
              </div>
            </RequireRole>
            <ProblemNotice error={actionError} testId="action-problem" />
            {modifying && (
              <ModifyForm
                id={id}
                onDone={(nd) => {
                  setD(nd);
                  setModifying(false);
                }}
              />
            )}
            <DecisionView d={d} />
          </>
        )}
      </RequireSession>
    </section>
  );
}
