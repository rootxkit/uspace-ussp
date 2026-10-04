"use client";

// Filing an operational intent (POST /v1/intents; 2021/664 Art. 6(4),
// Annex IV): the ten items numbered as in the Annex, the contingency
// measures, the emergency contact reference and the idempotency
// reference. The volume's outline is drawn on the map or typed, its
// altitudes are W84 metres as F3548 carries them; the API derives AMSL
// and judges everything (brief WP-17). The form checks shape only; every
// refusal the API names goes onto its field, the rest are listed with
// their path. A refused intent is a decision (201) whose conflicts the
// decision view lists, never a problem.
import { useCallback, useState } from "react";
import { useRouter } from "next/navigation";
import { useWatch } from "react-hook-form";
import { z } from "zod";
import { CheckboxField, EnumField, Form, NumberField, TextField, UTCDateTimeField } from "@rootxkit/uspace-ui/form";
import type { FieldError } from "@rootxkit/uspace-ui/model";
import { useAppT } from "@/i18n/t";
import { useApi, type Schemas } from "@/lib/api";
import { PageHeading, RequireSession } from "../common";
import { PortalMap } from "../PortalMap";
import { DraftLayer, OutlineFields, outlineOf, type Outline, type Point } from "./VolumeEditor";

const MODES = ["VLOS", "BVLOS"] as const;
const FLIGHT_TYPES = ["normal", "special_operation"] as const;
const CATEGORIES = ["open", "specific", "certified"] as const;
const SUBCATEGORIES = ["A1", "A2", "A3"] as const;
const CLASS_LABELS = ["C0", "C1", "C2", "C3", "C4", "C5", "C6"] as const;
const IDENT_TECH = ["network", "direct", "both"] as const;

const optionalText = (max: number) =>
  z
    .string()
    .trim()
    .max(max)
    .transform((v) => (v === "" ? undefined : v));

const band = z.object({
  altitude_lower: z.object({ value: z.number() }),
  altitude_upper: z.object({ value: z.number() }),
});

export const intentSchema = z.object({
  client_ref: z.string().trim().min(1).max(64),
  uas_serial: z.string().trim().min(1).max(64),
  mode: z.enum(MODES),
  flight_type: z.enum(FLIGHT_TYPES),
  priority: z.number().int().min(0).nullable(),
  category: z.enum(CATEGORIES),
  subcategory: z.enum(SUBCATEGORIES).nullable(),
  class_label: z.enum(CLASS_LABELS).nullable(),
  privately_built: z.boolean(),
  mtom_kg: z.number().positive().nullable(),
  volumes: z
    .array(
      z.object({
        volume: band,
        time_start: z.object({ value: z.string() }),
        time_end: z.object({ value: z.string() }),
      }),
    )
    .length(1),
  identification_technology: z.enum(IDENT_TECH),
  connectivity_methods: z.string().trim().min(1).max(256),
  endurance_s: z.number().int().positive(),
  loss_of_c2_procedure: z.string().trim().min(1).max(256),
  operator_reg: z.string().trim().min(1).max(64),
  ua_registration: optionalText(64),
  pilot_ref: optionalText(32),
  contingency: z.object({ procedure: z.string().trim().min(1).max(512) }),
  emergency_contact_ref: z.string().trim().min(1).max(64),
  authorisation_ref: optionalText(64),
});

type Values = z.output<typeof intentSchema>;
type Input = z.input<typeof intentSchema>;

/** A client reference unique to this filing: the API is idempotent on it per client. */
function newClientRef(): string {
  return `portal-${crypto.randomUUID()}`;
}

function defaults(): Input {
  return {
    client_ref: newClientRef(),
    uas_serial: "",
    mode: "VLOS",
    flight_type: "normal",
    priority: null,
    category: "open",
    subcategory: null,
    class_label: null,
    privately_built: false,
    mtom_kg: null,
    volumes: [
      {
        volume: { altitude_lower: { value: null as unknown as number }, altitude_upper: { value: null as unknown as number } },
        time_start: { value: null as unknown as string },
        time_end: { value: null as unknown as string },
      },
    ],
    identification_technology: "network",
    connectivity_methods: "",
    endurance_s: null as unknown as number,
    loss_of_c2_procedure: "",
    operator_reg: "",
    ua_registration: "",
    pilot_ref: "",
    contingency: { procedure: "" },
    emergency_contact_ref: "",
    authorisation_ref: "",
  };
}

/** The request body from the form's values and the drawn outline, as entered. */
export function requestOf(v: Values, outline: NonNullable<ReturnType<typeof outlineOf>>): Schemas["IntentRequest"] {
  const vol = v.volumes[0];
  if (vol === undefined) throw new Error("no volume");
  const body: Schemas["IntentRequest"] = {
    client_ref: v.client_ref,
    uas_serial: v.uas_serial,
    mode: v.mode,
    flight_type: v.flight_type,
    category: v.category,
    volumes: [
      {
        volume: {
          ...outline,
          altitude_lower: { value: vol.volume.altitude_lower.value, reference: "W84", units: "M" },
          altitude_upper: { value: vol.volume.altitude_upper.value, reference: "W84", units: "M" },
        },
        time_start: { value: vol.time_start.value, format: "RFC3339" },
        time_end: { value: vol.time_end.value, format: "RFC3339" },
      },
    ],
    identification_technology: v.identification_technology,
    connectivity_methods: v.connectivity_methods
      .split(",")
      .map((s) => s.trim())
      .filter((s) => s !== ""),
    endurance_s: v.endurance_s,
    loss_of_c2_procedure: v.loss_of_c2_procedure,
    operator_reg: v.operator_reg,
    contingency: { procedure: v.contingency.procedure },
    emergency_contact_ref: v.emergency_contact_ref,
  };
  if (v.priority !== null) body.priority = v.priority;
  if (v.subcategory !== null) body.subcategory = v.subcategory;
  if (v.class_label !== null) body.class_label = v.class_label;
  if (v.privately_built) body.privately_built = true;
  if (v.mtom_kg !== null) body.mtom_kg = v.mtom_kg;
  if (v.ua_registration !== undefined) body.ua_registration = v.ua_registration;
  if (v.pilot_ref !== undefined) body.pilot_ref = v.pilot_ref;
  if (v.authorisation_ref !== undefined) body.authorisation_ref = v.authorisation_ref;
  return body;
}

/** The priority only beside a special operation's flight type (Annex IV item 3). */
function PriorityField() {
  const flightType = useWatch({ name: "flight_type" }) as string | null;
  if (flightType !== "special_operation") return null;
  return <NumberField name="priority" labelKey="portal.intent.priority" hintKey="portal.intent.priority_hint" />;
}

/** The open category's subcategory (Annex IV item 4). */
function SubcategoryField() {
  const category = useWatch({ name: "category" }) as string | null;
  if (category !== "open") return null;
  return <EnumField name="subcategory" labelKey="portal.intent.subcategory" values={SUBCATEGORIES} i18nPrefix="portal.subcategory" />;
}

function ItemHeading({ n, children }: { n: number; children: string }) {
  const t = useAppT();
  return (
    <h2 className="m-0 mt-2 text-sm font-semibold">
      {t("portal.intent.item_heading", { n, item: children })}
    </h2>
  );
}

export function IntentFormPage() {
  const t = useAppT();
  const api = useApi();
  const router = useRouter();
  const [outline, setOutline] = useState<Outline>({ kind: "polygon", vertices: [] });
  const [initial] = useState(defaults);
  const onPick = useCallback(
    (p: Point) =>
      setOutline((o) => (o.kind === "polygon" ? { ...o, vertices: [...o.vertices, p] } : { ...o, center: p })),
    [],
  );
  return (
    <section aria-labelledby="new-intent-heading" className="flex flex-col gap-3">
      <PageHeading id="new-intent-heading">{t("portal.intent.new_heading")}</PageHeading>
      <p className="m-0 text-sm text-[var(--us-text-muted)]">{t("portal.intent.new_note")}</p>
      <RequireSession>
        <div className="grid gap-4 lg:grid-cols-[1fr_1fr]">
          <div className="flex flex-col gap-2">
            <PortalMap className="h-[360px] lg:h-[520px]">
              <DraftLayer outline={outline} onPick={onPick} />
            </PortalMap>
            <OutlineFields outline={outline} onChange={setOutline} />
          </div>
          <Form
            schema={intentSchema}
            defaults={initial}
            submitLabelKey="portal.intent.submit"
            busyLabelKey="portal.intent.submitting"
            onSubmit={async (v): Promise<FieldError[] | undefined> => {
              const o = outlineOf(outline);
              if (o === null) return [{ field: "volumes[0].volume", reason: t("portal.intent.outline.missing") }];
              const { data } = await api.POST("/v1/intents", { body: requestOf(v, o) });
              if (data !== undefined) router.push(`/intents/${data.intent_id}`);
              return undefined;
            }}
          >
            <TextField name="client_ref" labelKey="portal.intent.client_ref" hintKey="portal.intent.client_ref_hint" required />
            <ItemHeading n={1}>{t("portal.item.1")}</ItemHeading>
            <TextField name="uas_serial" labelKey="portal.intent.uas_serial" hintKey="portal.intent.uas_serial_hint" required />
            <ItemHeading n={2}>{t("portal.item.2")}</ItemHeading>
            <EnumField name="mode" labelKey="portal.intent.mode" values={MODES} i18nPrefix="portal.mode" required />
            <ItemHeading n={3}>{t("portal.item.3")}</ItemHeading>
            <EnumField name="flight_type" labelKey="portal.intent.flight_type" values={FLIGHT_TYPES} i18nPrefix="portal.flight_type" required />
            <PriorityField />
            <ItemHeading n={4}>{t("portal.item.4")}</ItemHeading>
            <EnumField name="category" labelKey="portal.intent.category" values={CATEGORIES} i18nPrefix="portal.category" required />
            <SubcategoryField />
            <EnumField name="class_label" labelKey="portal.intent.class_label" values={CLASS_LABELS} i18nPrefix="portal.class_label" />
            <NumberField name="mtom_kg" labelKey="portal.intent.mtom_kg" unit="form.unit.kg" />
            <CheckboxField name="privately_built" labelKey="portal.intent.privately_built" />
            <ItemHeading n={5}>{t("portal.item.5")}</ItemHeading>
            <p className="m-0 text-xs text-[var(--us-text-muted)]">{t("portal.intent.volume_hint")}</p>
            <NumberField name="volumes.0.volume.altitude_lower.value" labelKey="portal.intent.alt_lower" unit="form.unit.m" datum="WGS84" required />
            <NumberField name="volumes.0.volume.altitude_upper.value" labelKey="portal.intent.alt_upper" unit="form.unit.m" datum="WGS84" required />
            <UTCDateTimeField name="volumes.0.time_start.value" labelKey="portal.intent.time_start" required />
            <UTCDateTimeField name="volumes.0.time_end.value" labelKey="portal.intent.time_end" required />
            <ItemHeading n={6}>{t("portal.item.6")}</ItemHeading>
            <EnumField name="identification_technology" labelKey="portal.intent.identification_technology" values={IDENT_TECH} i18nPrefix="portal.ident_tech" required />
            <ItemHeading n={7}>{t("portal.item.7")}</ItemHeading>
            <TextField name="connectivity_methods" labelKey="portal.intent.connectivity_methods" hintKey="portal.intent.connectivity_methods_hint" required />
            <ItemHeading n={8}>{t("portal.item.8")}</ItemHeading>
            <NumberField name="endurance_s" labelKey="portal.intent.endurance_s" unit="form.unit.s" required />
            <ItemHeading n={9}>{t("portal.item.9")}</ItemHeading>
            <TextField name="loss_of_c2_procedure" labelKey="portal.intent.loss_of_c2_procedure" required />
            <ItemHeading n={10}>{t("portal.item.10")}</ItemHeading>
            <TextField name="operator_reg" labelKey="portal.intent.operator_reg" required />
            <TextField name="ua_registration" labelKey="portal.intent.ua_registration" />
            <h2 className="m-0 mt-2 text-sm font-semibold">{t("portal.intent.beside_items")}</h2>
            <TextField name="contingency.procedure" labelKey="portal.intent.contingency" hintKey="portal.intent.contingency_hint" required />
            <TextField name="emergency_contact_ref" labelKey="portal.intent.emergency_contact_ref" hintKey="portal.intent.emergency_contact_ref_hint" required />
            <TextField name="pilot_ref" labelKey="portal.intent.pilot_ref" />
            <TextField name="authorisation_ref" labelKey="portal.intent.authorisation_ref" />
          </Form>
        </div>
      </RequireSession>
    </section>
  );
}
