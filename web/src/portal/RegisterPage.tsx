"use client";

// Self-registration (POST /v1/accounts/operators): the registration
// number the authority issued, a display name, a contact email and the
// first portal user (operator_admin). The answer is the operator's state
// as the API decided it from the authority's registry (F8): active,
// pending_validation or refused, with what the registry answered, status
// only. The page shows it and decides nothing.
import { useState } from "react";
import Link from "next/link";
import { z } from "zod";
import { Form, TextField } from "@rootxkit/uspace-ui/form";
import { useAppT } from "@/i18n/t";
import { useApi, type Schemas } from "@/lib/api";
import { Fact, PageHeading, Utc } from "./common";

const schema = z.object({
  registration_number: z.string().trim().min(1).max(32),
  display_name: z.string().trim().min(1).max(200),
  contact_email: z.email().max(254),
  admin_username: z.string().trim().min(1).max(128),
  admin_password: z.string().min(1).max(1024),
});

export function RegisterPage() {
  const t = useAppT();
  const api = useApi();
  const [operator, setOperator] = useState<Schemas["Operator"] | null>(null);
  return (
    <section aria-labelledby="register-heading" className="flex max-w-xl flex-col gap-3">
      <PageHeading id="register-heading">{t("portal.register.heading")}</PageHeading>
      <p className="m-0 text-sm text-[var(--us-text-muted)]">{t("portal.register.note")}</p>
      <Form
        schema={schema}
        defaults={{ registration_number: "", display_name: "", contact_email: "", admin_username: "", admin_password: "" }}
        submitLabelKey="portal.register.submit"
        onSubmit={async (v) => {
          const { data } = await api.POST("/v1/accounts/operators", { body: v });
          setOperator(data ?? null);
        }}
      >
        <TextField name="registration_number" labelKey="portal.register.registration_number" hintKey="portal.register.registration_number_hint" required />
        <TextField name="display_name" labelKey="portal.register.display_name" required />
        <TextField name="contact_email" labelKey="portal.register.contact_email" type="email" autoComplete="email" required />
        <TextField name="admin_username" labelKey="portal.register.admin_username" autoComplete="username" required />
        <TextField name="admin_password" labelKey="portal.register.admin_password" autoComplete="new-password" required />
      </Form>
      {operator !== null && (
        <section aria-labelledby="registered-heading" role="status" data-testid="registered" data-status={operator.status} className="rounded border border-[var(--us-border)] p-3">
          <h2 id="registered-heading" className="m-0 text-base font-semibold">
            {t(`portal.operator.status.${operator.status}`)}
          </h2>
          <dl className="m-0 mt-2 grid gap-2 sm:grid-cols-2">
            <Fact term={t("portal.register.registration_number")}>{operator.registration_number}</Fact>
            <Fact term={t("portal.operator.validation_status")} testId="validation-status">
              {operator.validation_status ?? t("portal.operator.validation_none")}
            </Fact>
            <Fact term={t("portal.operator.validated_at")}>
              <Utc iso={operator.validated_at} />
            </Fact>
            <Fact term={t("portal.operator.created_at")}>
              <Utc iso={operator.created_at} />
            </Fact>
          </dl>
          <p className="m-0 mt-2 text-sm">{t(`portal.operator.next.${operator.status}`)}</p>
          <Link href="/login" className="underline">
            {t("portal.nav.login")}
          </Link>
        </section>
      )}
    </section>
  );
}
